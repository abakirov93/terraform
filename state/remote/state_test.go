package remote

import (
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/states/statemgr"
	"github.com/hashicorp/terraform/version"
)

func TestState_impl(t *testing.T) {
	var _ statemgr.Reader = new(State)
	var _ statemgr.Writer = new(State)
	var _ statemgr.Persister = new(State)
	var _ statemgr.Refresher = new(State)
	var _ statemgr.Locker = new(State)
}

func TestStateRace(t *testing.T) {
	s := &State{
		Client: nilClient{},
	}

	current := states.NewState()

	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.WriteState(current)
			s.PersistState()
			s.RefreshState()
		}()
	}
	wg.Wait()
}

// testCase encapsulates a test state test
type testCase struct {
	name string
	// A function to mutate state and return a cleanup function
	mutationFunc func(*State) (*states.State, func())
	// The expected request to have taken place
	expectedRequest mockClientRequest
	// Mark this case as not having a request
	noRequest bool
}

// isRequested ensures a test that is specified as not having
// a request doesn't have one by checking if a method exists
// on the expectedRequest.
func (tc testCase) isRequested(t *testing.T) bool {
	hasMethod := tc.expectedRequest.Method != ""
	if tc.noRequest && hasMethod {
		t.Fatalf("expected no content for %q but got: %v", tc.name, tc.expectedRequest)
	}
	return !tc.noRequest
}

func TestStatePersist(t *testing.T) {
	testCases := []testCase{
		// Refreshing state before we run the test loop causes a GET
		testCase{
			name: "refresh state",
			mutationFunc: func(mgr *State) (*states.State, func()) {
				return mgr.State(), func() {}
			},
			expectedRequest: mockClientRequest{
				Method: "Get",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            1.0, // encoding/json decodes this as float64 by default
					"terraform_version": "0.0.0",
					"outputs":           map[string]interface{}{},
					"resources":         []interface{}{},
				},
			},
		},
		testCase{
			name: "change lineage",
			mutationFunc: func(mgr *State) (*states.State, func()) {
				originalLineage := mgr.lineage
				mgr.lineage = "some-new-lineage"
				return mgr.State(), func() {
					mgr.lineage = originalLineage
				}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "some-new-lineage",
					"serial":            2.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs":           map[string]interface{}{},
					"resources":         []interface{}{},
				},
			},
		},
		testCase{
			name: "change serial",
			mutationFunc: func(mgr *State) (*states.State, func()) {
				originalSerial := mgr.serial
				mgr.serial++
				return mgr.State(), func() {
					mgr.serial = originalSerial
				}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            4.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs":           map[string]interface{}{},
					"resources":         []interface{}{},
				},
			},
		},
		testCase{
			name: "add output to state",
			mutationFunc: func(mgr *State) (*states.State, func()) {
				s := mgr.State()
				s.RootModule().SetOutputValue("foo", cty.StringVal("bar"), false)
				return s, func() {}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            3.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs": map[string]interface{}{
						"foo": map[string]interface{}{
							"type":  "string",
							"value": "bar",
						},
					},
					"resources": []interface{}{},
				},
			},
		},
		testCase{
			name: "mutate state bar -> baz",
			mutationFunc: func(mgr *State) (*states.State, func()) {
				s := mgr.State()
				s.RootModule().SetOutputValue("foo", cty.StringVal("baz"), false)
				return s, func() {}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            4.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs": map[string]interface{}{
						"foo": map[string]interface{}{
							"type":  "string",
							"value": "baz",
						},
					},
					"resources": []interface{}{},
				},
			},
		},
		testCase{
			name: "nothing changed",
			mutationFunc: func(mgr *State) (*states.State, func()) {
				s := mgr.State()
				return s, func() {}
			},
			noRequest: true,
		},
	}

	// Initial setup of state just to give us a fixed starting point for our
	// test assertions below, or else we'd need to deal with
	// random lineage.
	mgr := &State{
		Client: &mockClient{
			current: []byte(`
				{
					"version": 4,
					"lineage": "mock-lineage",
					"serial": 1,
					"terraform_version":"0.0.0",
					"outputs": {},
					"resources": []
				}
			`),
		},
	}

	// In normal use (during a Terraform operation) we always refresh and read
	// before any writes would happen, so we'll mimic that here for realism.
	// NB This causes a GET to be logged so the first item in the test cases
	// must account for this
	if err := mgr.RefreshState(); err != nil {
		t.Fatalf("failed to RefreshState: %s", err)
	}

	// Our client is a mockClient which has a log we
	// use to check that operations generate expected requests
	mockClient := mgr.Client.(*mockClient)

	// logIdx tracks the current index of the log separate from
	// the loop iteration so we can check operations that don't
	// cause any requests to be generated
	logIdx := 0

	// Run tests in order.
	for _, tc := range testCases {
		s, cleanup := tc.mutationFunc(mgr)

		if err := mgr.WriteState(s); err != nil {
			t.Fatalf("failed to WriteState for %q: %s", tc.name, err)
		}
		if err := mgr.PersistState(); err != nil {
			t.Fatalf("failed to PersistState for %q: %s", tc.name, err)
		}

		if tc.isRequested(t) {
			// Get captured request from the mock client log
			// based on the index of the current test
			loggedRequest := mockClient.log[logIdx]
			logIdx++
			if diff := cmp.Diff(tc.expectedRequest, loggedRequest); len(diff) > 0 {
				t.Fatalf("incorrect client requests for %q:\n%s", tc.name, diff)
			}
		}
		cleanup()
	}
}
