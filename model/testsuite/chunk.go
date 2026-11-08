package testsuite

import (
	"testing"

	"github.com/bornholm/amoxtli/model"
)

// TestChunk checks the contract of model.Document.Chunk. Chunk clamps a range
// that runs out of the content and returns an error for an inverted one,
// where slicing would panic. factory must return a document holding data.
func TestChunk(t *testing.T, factory func(t *testing.T, data []byte) model.Document) {
	type testCase struct {
		Name          string
		Start         int
		End           int
		Expected      string
		ExpectedError bool
	}

	testCases := []testCase{
		{Name: "within range", Start: 1, End: 4, Expected: "ell"},
		{Name: "negative start", Start: -3, End: 2, Expected: "he"},
		{Name: "end past data", Start: 3, End: 42, Expected: "lo"},
		{Name: "empty range", Start: 2, End: 2, Expected: ""},
		{Name: "start past end", Start: 4, End: 1, ExpectedError: true},
		{Name: "start past data", Start: 10, End: 20, ExpectedError: true},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			doc := factory(t, []byte("hello"))

			chunk, err := doc.Chunk(tc.Start, tc.End)

			if tc.ExpectedError {
				if err == nil {
					t.Errorf("expected an error, got chunk %q", chunk)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %+v", err)
			}

			if e, g := tc.Expected, string(chunk); e != g {
				t.Errorf("chunk: expected %q, got %q", e, g)
			}
		})
	}
}
