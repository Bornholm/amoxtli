// Package retrieval implements grounding-driven retrieval orchestration ported
// from Corpus' MothRAG-derived mechanisms: an evidence evaluator (relevance
// filtering fused with a grounding (γ) verdict), query reformulation for
// iterative re-retrieval, and query decomposition. It layers on top of
// amoxtli's Search without introducing any answer-generation step — callers
// receive the fused evidence and the grounding verdict and decide themselves
// whether to abstain or re-query.
package retrieval

import (
	"context"
	"strings"

	"github.com/bornholm/amoxtli/model"
)

// SectionStore provides access to the content of indexed sections. It is
// satisfied structurally by ingest.Store.
type SectionStore interface {
	GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error)
}

// GroundingStatus qualifies whether the retrieved evidence supports a reliable,
// grounded answer to a query. It is the discrete form of the "γ" signal ported
// from MothRAG.
type GroundingStatus string

const (
	GroundingValid   GroundingStatus = "valid"
	GroundingPartial GroundingStatus = "partial"
	GroundingInvalid GroundingStatus = "invalid"
)

// GroundingResult is the sufficiency verdict over a set of evidence.
type GroundingResult struct {
	Status      GroundingStatus `json:"status"`
	Score       float64         `json:"score"`
	Explanation string          `json:"explanation"`
}

func normalizeGroundingStatus(s string) GroundingStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "valid":
		return GroundingValid
	case "partial":
		return GroundingPartial
	default:
		return GroundingInvalid
	}
}

// clampScore constrains a confidence score to the [0,1] range.
func clampScore(score float64) float64 {
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}
