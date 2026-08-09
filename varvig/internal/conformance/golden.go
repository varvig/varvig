package conformance

import (
	_ "embed"
	"encoding/json"
)

// goldenJSON is the checked-in conformance artifact. It is a content-addressed
// object in the repository (design §4.7): its identity is GoldenSuiteID, and
// every release's build must satisfy it. Regenerate with
// `VARVIG_WRITE_GOLDEN=1 go test ./internal/conformance/ -run TestWriteGolden`.
//
//go:embed vectors.json
var goldenJSON []byte

// GoldenSuiteID is the multihash of vectors.json — the suite's stable identity.
// A change here means the frozen format's golden artifact changed, which must
// be a deliberate, reviewed event.
const GoldenSuiteID = "1e205a696b35bede169782d371fd80c8ca63b4b73a746c2edff95575651cbf0b7980"

// Golden returns the parsed golden suite.
func Golden() Vectors {
	var v Vectors
	if err := json.Unmarshal(goldenJSON, &v); err != nil {
		panic("conformance: corrupt embedded golden vectors: " + err.Error())
	}
	return v
}

// GoldenBytes returns the raw golden artifact bytes.
func GoldenBytes() []byte { return goldenJSON }
