package mcpserver

import (
	"context"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/internal/filterexpr"
	"github.com/bornholm/amoxtli/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
)

// --- tool payloads -------------------------------------------------------

type searchInput struct {
	Query       string   `json:"query" jsonschema:"the search query"`
	MaxResults  int      `json:"max_results,omitempty" jsonschema:"maximum number of results (default 5)"`
	Collections []string `json:"collections,omitempty" jsonschema:"restrict to these collection labels or IDs"`
	Deep        bool     `json:"deep,omitempty" jsonschema:"run iterative LLM-driven retrieval (requires a configured chat model)"`
	Filters     []string `json:"filters,omitempty" jsonschema:"metadata filter expressions (key=value, key!=value, key>=value...), e.g. type=code, language=go, or type!=code for documentation only"`
}

type sectionResult struct {
	ID      string `json:"id"`
	Content string `json:"content,omitempty"`
}

type documentResult struct {
	Source   string          `json:"source"`
	Score    float64         `json:"score"`
	Sections []sectionResult `json:"sections"`
}

type groundingResult struct {
	Status      string  `json:"status"`
	Score       float64 `json:"score"`
	Explanation string  `json:"explanation,omitempty"`
}

type searchOutput struct {
	Results   []documentResult `json:"results"`
	Grounding *groundingResult `json:"grounding,omitempty"`
	Rounds    int              `json:"rounds,omitempty"`
}

type fetchSectionsInput struct {
	SectionIDs []string `json:"section_ids" jsonschema:"the section IDs to fetch"`
}

type fetchSectionsOutput struct {
	Sections map[string]string `json:"sections"`
}

type listCollectionsOutput struct {
	Collections []collectionResult `json:"collections"`
}

type collectionResult struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type listDocumentsInput struct {
	Collection string `json:"collection,omitempty" jsonschema:"restrict to a collection label or ID"`
	SourceLike string `json:"source_like,omitempty" jsonschema:"only documents whose source matches this pattern"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum number of documents (default 50)"`
}

type listDocumentsOutput struct {
	Documents []documentHeader `json:"documents"`
	Total     int64            `json:"total"`
}

type documentHeader struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

// --- registration --------------------------------------------------------

// registerTools wires the read-only tools. hasChat gates the deep-search
// capability; groundingEnabled adds a confidence verdict to plain searches.
func (s *Server) registerTools(hasChat, groundingEnabled bool) {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "search",
		Description: "Search the local document corpus. Returns matching documents with their most relevant sections inline. Indexed source code carries type=code and language=<name> metadata: use filters like [\"type=code\"] to search code only, or [\"type!=code\"] for documentation only.",
	}, s.handleSearch(hasChat, groundingEnabled))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "fetch_sections",
		Description: "Fetch the full content of specific sections by ID (e.g. to expand on a search result).",
	}, s.handleFetchSections)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_collections",
		Description: "List the collections available in the workspace.",
	}, s.handleListCollections)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_documents",
		Description: "List indexed documents, optionally restricted to a collection or source pattern.",
	}, s.handleListDocuments)
}

func (s *Server) handleSearch(hasChat, groundingEnabled bool) mcp.ToolHandlerFor[searchInput, searchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
		if in.Query == "" {
			return nil, searchOutput{}, errors.New("query is required")
		}
		if in.Deep && !hasChat {
			return nil, searchOutput{}, errors.New("deep search requires a chat model configured in the workspace (llm.chat)")
		}

		maxResults := in.MaxResults
		if maxResults <= 0 {
			maxResults = 5
		}

		collections, err := s.rt.ResolveCollections(ctx, in.Collections, false)
		if err != nil {
			return nil, searchOutput{}, err
		}

		opts := []amoxtli.SearchOption{amoxtli.WithSearchMaxResults(maxResults)}
		if len(collections) > 0 {
			opts = append(opts, amoxtli.WithSearchCollections(collections...))
		}

		if len(in.Filters) > 0 {
			conditions, err := filterexpr.ParseFilters(in.Filters)
			if err != nil {
				return nil, searchOutput{}, err
			}

			opts = append(opts, amoxtli.WithSearchFilter(conditions...))
		}

		out := searchOutput{}

		if in.Deep {
			result, err := s.rt.Codex.SearchIterative(ctx, in.Query, opts...)
			if err != nil {
				return nil, searchOutput{}, errors.WithStack(err)
			}

			out.Results, err = s.renderResults(ctx, result.Results)
			if err != nil {
				return nil, searchOutput{}, err
			}
			if result.Grounding != nil {
				out.Grounding = &groundingResult{
					Status:      string(result.Grounding.Status),
					Score:       result.Grounding.Score,
					Explanation: result.Grounding.Explanation,
				}
			}
			out.Rounds = result.Rounds
		} else {
			results, err := s.rt.Codex.Search(ctx, in.Query, opts...)
			if err != nil {
				return nil, searchOutput{}, errors.WithStack(err)
			}

			out.Results, err = s.renderResults(ctx, results)
			if err != nil {
				return nil, searchOutput{}, err
			}

			// Surface the grounding confidence verdict when grounding is
			// enabled (an extra LLM evaluation over the returned evidence).
			if groundingEnabled {
				grounding, err := s.rt.Codex.CheckGrounding(ctx, in.Query, results)
				if err != nil {
					return nil, searchOutput{}, errors.WithStack(err)
				}
				out.Grounding = &groundingResult{
					Status:      string(grounding.Status),
					Score:       grounding.Score,
					Explanation: grounding.Explanation,
				}
			}
		}

		return nil, out, nil
	}
}

func (s *Server) handleFetchSections(ctx context.Context, _ *mcp.CallToolRequest, in fetchSectionsInput) (*mcp.CallToolResult, fetchSectionsOutput, error) {
	ids := make([]model.SectionID, 0, len(in.SectionIDs))
	for _, id := range in.SectionIDs {
		ids = append(ids, model.SectionID(id))
	}

	sections, err := s.rt.Codex.GetSectionsByIDs(ctx, ids)
	if err != nil {
		return nil, fetchSectionsOutput{}, errors.WithStack(err)
	}

	out := fetchSectionsOutput{Sections: map[string]string{}}
	for id, section := range sections {
		content, err := section.Content()
		if err != nil {
			return nil, fetchSectionsOutput{}, errors.WithStack(err)
		}
		out.Sections[string(id)] = string(content)
	}

	return nil, out, nil
}

func (s *Server) handleListCollections(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listCollectionsOutput, error) {
	colls, err := s.rt.Store.QueryCollections(ctx, ingest.QueryCollectionsOptions{})
	if err != nil {
		return nil, listCollectionsOutput{}, errors.WithStack(err)
	}

	out := listCollectionsOutput{Collections: make([]collectionResult, 0, len(colls))}
	for _, coll := range colls {
		out.Collections = append(out.Collections, collectionResult{
			ID:          string(coll.ID()),
			Label:       coll.Label(),
			Description: coll.Description(),
		})
	}

	return nil, out, nil
}

func (s *Server) handleListDocuments(ctx context.Context, _ *mcp.CallToolRequest, in listDocumentsInput) (*mcp.CallToolResult, listDocumentsOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	query := ingest.QueryDocumentsOptions{HeaderOnly: true, Limit: &limit}
	if in.SourceLike != "" {
		query.SourcePattern = &in.SourceLike
	}

	var (
		docs  []model.PersistedDocument
		total int64
		err   error
	)

	if in.Collection != "" {
		collID, resolveErr := s.rt.ResolveCollection(ctx, in.Collection, false)
		if resolveErr != nil {
			return nil, listDocumentsOutput{}, resolveErr
		}
		docs, total, err = s.rt.Store.QueryDocumentsByCollectionID(ctx, collID, query)
	} else {
		docs, total, err = s.rt.Store.QueryDocuments(ctx, query)
	}
	if err != nil {
		return nil, listDocumentsOutput{}, errors.WithStack(err)
	}

	out := listDocumentsOutput{Total: total, Documents: make([]documentHeader, 0, len(docs))}
	for _, doc := range docs {
		header := documentHeader{ID: string(doc.ID())}
		if doc.Source() != nil {
			header.Source = doc.Source().String()
		}
		out.Documents = append(out.Documents, header)
	}

	return nil, out, nil
}

// renderResults fetches section contents inline so the agent gets usable
// evidence in a single round-trip.
func (s *Server) renderResults(ctx context.Context, results []*index.SearchResult) ([]documentResult, error) {
	ids := []model.SectionID{}
	for _, r := range results {
		ids = append(ids, r.Sections...)
	}

	sections, err := s.rt.Codex.GetSectionsByIDs(ctx, ids)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	rendered := make([]documentResult, 0, len(results))
	for _, r := range results {
		doc := documentResult{Score: r.Score, Sections: make([]sectionResult, 0, len(r.Sections))}
		if r.Source != nil {
			doc.Source = r.Source.String()
		}

		for _, id := range r.Sections {
			section := sectionResult{ID: string(id)}
			if s, ok := sections[id]; ok {
				content, err := s.Content()
				if err != nil {
					return nil, errors.WithStack(err)
				}
				section.Content = string(content)
			}
			doc.Sections = append(doc.Sections, section)
		}

		rendered = append(rendered, doc)
	}

	return rendered, nil
}
