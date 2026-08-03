package runner

import (
	"strings"
	"testing"

	"github.com/kaessert/crossplane-update-tester/internal/manifest"
)

// TestBuildResolveRecoverResult proves the two independent recovery signals
// documented on RunResolveRecover are both real gates, not decisions that
// can only ever pass. In particular, "SilentDuplicateStillCorrectlyPrefixed"
// is the scenario produced by forcing the identity search to run against
// the wrong one of the two backend object types a single kind can map to:
// the duplicate object's external-name is STILL correctly prefixed (Create
// derives the object type from the desired spec, not from how identity was
// searched), so the prefix check alone cannot catch it — only the
// CreatedExternalResource event count can.
func TestBuildResolveRecoverResult(t *testing.T) {
	const prefix = testObjectTypePrefix

	cases := map[string]struct {
		reason         string
		before         string
		after          string
		expectedPrefix string
		createEvents   int
		wantPassed     bool
		wantDiagHas    string
	}{
		"RecoveredToSameObject": {
			reason:         "identity resolved via search to the SAME object: prefix still matches and exactly one create event was ever recorded",
			before:         prefix + "abc123/example-object",
			after:          prefix + "abc123/example-object",
			expectedPrefix: prefix,
			createEvents:   1,
			wantPassed:     true,
		},
		"SilentDuplicateStillCorrectlyPrefixed": {
			reason:         "the wrong-object-type search scenario: the search finds nothing, the reconciler creates a SECOND object, and that duplicate's external-name is still correctly prefixed because Create derives the object type from the desired spec — the prefix check alone would wrongly pass this, so only the event count may fail it",
			before:         prefix + "abc123/example-object",
			after:          prefix + "def456/example-object",
			expectedPrefix: prefix,
			createEvents:   2,
			wantPassed:     false,
			wantDiagHas:    "2 CreatedExternalResource event(s)",
		},
		"WrongPrefixAfterRecovery": {
			reason:         "a recovered external-name that resolved against the sibling object type entirely must fail, independent of the event-count signal",
			before:         prefix + "abc123/example-object",
			after:          testSiblingObjectTypePrefix + "abc123/example-object",
			expectedPrefix: prefix,
			createEvents:   1,
			wantPassed:     false,
			wantDiagHas:    "recovered external-name failed prefix check",
		},
		"ZeroCreateEventsIsAlsoAFailure": {
			reason:         "an event count of zero is just as much a failure as two — it means the create event was never observed and cannot be trusted as a single-create proof either",
			before:         prefix + "abc123/example-object",
			after:          prefix + "abc123/example-object",
			expectedPrefix: prefix,
			createEvents:   0,
			wantPassed:     false,
			wantDiagHas:    "0 CreatedExternalResource event(s)",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := buildResolveRecoverResult(tc.before, tc.after, tc.expectedPrefix, tc.createEvents)
			if result.Passed != tc.wantPassed {
				t.Fatalf("%s: Passed = %v, want %v (message: %s, diagnostics: %v)",
					tc.reason, result.Passed, tc.wantPassed, result.Message, result.Diagnostics)
			}
			if result.ExternalNameBefore != tc.before || result.ExternalNameAfter != tc.after || result.CreateEventCount != tc.createEvents {
				t.Errorf("%s: result did not echo back the inputs: %+v", tc.reason, result)
			}
			if tc.wantDiagHas != "" {
				found := false
				for _, d := range result.Diagnostics {
					if strings.Contains(d, tc.wantDiagHas) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: diagnostics %v do not contain expected substring %q", tc.reason, result.Diagnostics, tc.wantDiagHas)
				}
			}
		})
	}
}

// TestRunResolveRecoverRequiresExpectedPrefix proves the guard fires before
// any kubectl call is attempted: a manifest that never declared
// crossplane.io/expect-external-name-prefix has no recovery contract to
// check, so RunResolveRecover must refuse it outright rather than silently
// no-op.
func TestRunResolveRecoverRequiresExpectedPrefix(t *testing.T) {
	r := &Runner{
		execFunc: func(args []string) (string, error) {
			t.Fatalf("unexpected kubectl call %v — the guard must fire before any exec", args)
			return "", nil
		},
	}
	m := &manifest.Manifest{Kind: testKindExample, Name: testNameExample}

	_, err := r.RunResolveRecover(m)
	if err == nil {
		t.Fatal("expected an error when the manifest has no expect-external-name-prefix annotation")
	}
	if !strings.Contains(err.Error(), manifest.ExpectExternalNamePrefixKey) {
		t.Errorf("error = %q, want it to name the missing annotation %q", err.Error(), manifest.ExpectExternalNamePrefixKey)
	}
}

// TestRunResolveRecoverRequiresExistingExternalName proves the second guard:
// stripping an external-name that was never set proves nothing, so
// RunResolveRecover must refuse to run against a resource that has not yet
// resolved an identity.
func TestRunResolveRecoverRequiresExistingExternalName(t *testing.T) {
	f := &fakeCluster{externalName: ""}
	r := newFakeRunner(f)
	m := &manifest.Manifest{
		Kind:                     testKindExample,
		Name:                     testNameExample,
		ExpectExternalNamePrefix: testObjectTypePrefix,
	}

	_, err := r.RunResolveRecover(m)
	if err == nil {
		t.Fatal("expected an error when the resource has no external-name to strip yet")
	}
	if !strings.Contains(err.Error(), "no external-name to strip") {
		t.Errorf("error = %q, want it to explain there is nothing to strip yet", err.Error())
	}
}

// TestSumEventOccurrencesByReasonCreated proves countCreateEvents' backing
// function only matches CreatedExternalResource events, ignoring
// update-related reasons for the same object — a reason mismatch here would
// silently let an unrelated Update event inflate the create count and mask
// a real duplicate-create bug.
func TestSumEventOccurrencesByReasonCreated(t *testing.T) {
	cases := map[string]struct {
		reason string
		list   eventList
		want   int
	}{
		"SingleCreateEvent": {
			reason: "the expected, healthy case: exactly one CreatedExternalResource event",
			list: eventList{Items: []eventItem{
				newTestEventItem(EventReasonCreated, 0, testKindExample, testNameExample),
			}},
			want: 1,
		},
		"TwoCreateEventsIsTheDuplicateSignature": {
			reason: "an aggregated count of 2 is exactly the silent-duplicate signature the resolve-recover check exists to catch",
			list: eventList{Items: []eventItem{
				newTestEventItem(EventReasonCreated, 2, testKindExample, testNameExample),
			}},
			want: 2,
		},
		"UpdateEventsAreNotCounted": {
			reason: "update-related events for the same object must never inflate the create count",
			list: eventList{Items: []eventItem{
				newTestEventItem(EventReasonCreated, 0, testKindExample, testNameExample),
				newTestEventItem(eventReasonUpdated, 5, testKindExample, testNameExample),
				newTestEventItem(eventReasonCannotUpdate, 3, testKindExample, testNameExample),
			}},
			want: 1,
		},
		"OtherObjectIgnored": {
			reason: "a CreatedExternalResource event for a different object must not count toward this resource's total",
			list: eventList{Items: []eventItem{
				newTestEventItem(EventReasonCreated, 0, testKindExample, testNameOtherInstance),
			}},
			want: 0,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := sumEventOccurrencesByReason(tc.list, testKindExample, testNameExample, EventReasonCreated)
			if got != tc.want {
				t.Errorf("%s: sumEventOccurrencesByReason() = %d, want %d", tc.reason, got, tc.want)
			}
		})
	}
}
