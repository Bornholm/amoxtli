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
	Filters     []string `json:"filters,omitempty" jsonschema:"metadata filter expressions (key=value, key!=value, key>=value..., key? if the key is set, !key if it is not), e.g. type=code, language=go, or !type for documents carrying no type. Every operator except !key requires the key to be present, so key!=value never matches a document lacking the key"`
}

type sectionResult struct {
	ID      string `json:"id"`
	Content string `json:"content,omitempty"`
}

type documentResult struct {
	Source string  `json:"source"`
	Score  float64 `json:"score"`
	// Metadata is the indexed document's metadata, echoed back so the agent can
	// see which keys and values are available to the filters parameter instead
	// of guessing them.
	Metadata map[string]any  `json:"metadata,omitempty"`
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
	ID       string         `json:"id"`
	Source   string         `json:"source"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// --- registration --------------------------------------------------------

// registerTools wires the read-only tools. Search depth is a workspace
// decision, not an agent one: iterative selects the grounding-driven
// re-retrieval orchestration and groundingEnabled surfaces the confidence
// verdict, both derived from the configured retrieval profile.
func (s *Server) registerTools(iterative, groundingEnabled bool) {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "search",
		Description: "Search the local document corpus. Returns matching documents with their most relevant sections inline, plus the metadata each document carries — read it to learn which keys and values the filters parameter accepts. Indexed source code carries type=code and language=<name> metadata: use filters like [\"type=code\"] to search code only, or [\"!type\"] for documentation only (every operator but !key requires the key to be present, so type!=code skips documents carrying no type at all).",
	}, s.handleSearch(iterative, groundingEnabled))

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

func (s *Server) handleSearch(iterative, groundingEnabled bool) mcp.ToolHandlerFor[searchInput, searchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
		if in.Query == "" {
			return nil, searchOutput{}, errors.New("query is required")
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

		if iterative {
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
			page, err := s.rt.Codex.SearchPage(ctx, in.Query, opts...)
			if err != nil {
				return nil, searchOutput{}, errors.WithStack(err)
			}

			out.Results, err = s.renderResults(ctx, page.Results)
			if err != nil {
				return nil, searchOutput{}, err
			}

			// Surface the grounding confidence verdict when grounding is
			// enabled. It was already computed during SearchPage, so reuse it
			// rather than paying for a second LLM evaluation.
			if groundingEnabled && page.Grounding != nil {
				out.Grounding = &groundingResult{
					Status:      string(page.Grounding.Status),
					Score:       page.Grounding.Score,
					Explanation: page.Grounding.Explanation,
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
		header := documentHeader{ID: string(doc.ID()), Metadata: model.Metadata(doc)}
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

				// Every section of a result belongs to the same document, so the
				// first one that carries metadata settles it. Content() above
				// already dereferences that same parent document.
				if doc.Metadata == nil {
					doc.Metadata = model.Metadata(s.Document())
				}
			}
			doc.Sections = append(doc.Sections, section)
		}

		rendered = append(rendered, doc)
	}

	return rendered, nil
}
