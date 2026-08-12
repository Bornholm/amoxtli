package mcpserver

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/blob"
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
	// MaxSectionsPerResult lets an agent widen the server-side bound for one
	// call, when an excerpt of a document is not enough to answer.
	MaxSectionsPerResult int      `json:"max_sections_per_result,omitempty" jsonschema:"maximum number of sections returned per matched document (defaults to the server setting); the omitted ones remain reachable through fetch_sections"`
	Filters              []string `json:"filters,omitempty" jsonschema:"metadata filter expressions (key=value, key!=value, key>=value..., key? if the key is set, !key if it is not), e.g. type=code, language=go, or !type for documents carrying no type. Every operator except !key requires the key to be present, so key!=value never matches a document lacking the key"`
}

type sectionResult struct {
	ID      string `json:"id"`
	Content string `json:"content,omitempty"`
	// TotalLength and NextOffset are set only when the content was cut to fit
	// the response budget, and say where to resume with fetch_sections. A
	// section returned whole carries neither, so an untruncated result reads
	// exactly as it did before.
	TotalLength int `json:"total_length,omitempty"`
	NextOffset  int `json:"next_offset,omitempty"`
}

type documentResult struct {
	Source string  `json:"source"`
	Score  float64 `json:"score"`
	// Metadata is the indexed document's metadata, echoed back so the agent can
	// see which keys and values are available to the filters parameter instead
	// of guessing them.
	Metadata map[string]any  `json:"metadata,omitempty"`
	Sections []sectionResult `json:"sections"`
	// OmittedSections counts the matched sections left out of this result by
	// the max_sections_per_result bound. Reported so that a trimmed result is
	// never mistaken for an exhaustive one.
	OmittedSections int `json:"omitted_sections,omitempty"`
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
	// Offset and Length page through a section whose content is too large to
	// survive the client's tool-result size limit in one piece.
	Offset int `json:"offset,omitempty" jsonschema:"start reading each section at this character offset (0-based, defaults to the beginning); use the next_offset reported by a previous call to continue where it stopped"`
	Length int `json:"length,omitempty" jsonschema:"maximum number of characters returned per section (0 or less returns the section up to its end)"`
}

// sectionContent carries a slice of a section's content along with enough
// bookkeeping for the agent to know whether it read the whole thing, and where
// to resume if it did not.
type sectionContent struct {
	Content string `json:"content"`
	// Offset is where this slice starts and Length how many characters it
	// holds, both in characters and not bytes, so that a follow-up call can be
	// addressed in the same units the request used.
	Offset int `json:"offset"`
	Length int `json:"length"`
	// TotalLength is the section's full size, so a partial read is never
	// mistaken for the whole section.
	TotalLength int `json:"total_length"`
	// NextOffset is set when content remains past this slice, and is the offset
	// to pass back to read the rest.
	NextOffset int `json:"next_offset,omitempty"`
}

type fetchSectionsOutput struct {
	Sections map[string]sectionContent `json:"sections"`
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
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum number of documents (default 20)"`
	// Page walks a corpus larger than one response can carry, rather than
	// having the client cut the listing mid-document.
	Page int `json:"page,omitempty" jsonschema:"0-based page index, skipping page*limit documents (defaults to the first page); compare the number of documents returned with total to know whether another page follows"`
}

type listDocumentsOutput struct {
	Documents []documentHeader `json:"documents"`
	Total     int64            `json:"total"`
}

type fetchImageInput struct {
	// URI is an amoxtli://images/<hash> reference as found in the content of a
	// section — a bare hash is accepted too.
	URI string `json:"uri" jsonschema:"the amoxtli://images/<hash> URI of the image, as found in a section content (a bare hash also works)"`
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
	searchDescription := "Search the local document corpus. Returns matching documents with their most relevant sections inline, plus the metadata each document carries — read it to learn which keys and values the filters parameter accepts. Indexed source code carries type=code and language=<name> metadata: use filters like [\"type=code\"] to search code only, or [\"!type\"] for documentation only (every operator but !key requires the key to be present, so type!=code skips documents carrying no type at all)."

	// A bounded result is announced as such: an agent told that a document has
	// more to give can go and get it, where one left to assume it saw
	// everything would answer from an excerpt without knowing.
	if s.maxSectionsPerResult > 0 {
		searchDescription += " Only the best scoring sections of each document are returned inline; omitted_sections counts those left out, which you can retrieve by ID with fetch_sections or by raising max_sections_per_result. A document showing omitted_sections has more to say than what you were given."
	}

	// A section cut to fit the response budget says so, for the same reason a
	// trimmed list of sections does: the agent must be able to tell an excerpt
	// from a whole section, and know how to get the rest.
	if s.maxContentChars > 0 {
		searchDescription += " Section contents are subject to a size budget for the whole response, spent on the best scoring sections first: a section carrying total_length was not returned in full — it may even come back with no content at all — and fetch_sections returns the rest, from next_offset."
	}

	// Images are described in text at index time; the description sits next to
	// an amoxtli://images/<hash> reference the agent can dereference.
	if s.rt.Codex.Blobs() != nil {
		searchDescription += " Section contents may reference images as amoxtli://images/<hash>: pass that URI to fetch_image to get the image itself."
	}

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "search",
		Description: searchDescription,
	}, s.handleSearch(iterative, groundingEnabled))

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "fetch_sections",
		Description: "Fetch the content of specific sections by ID (e.g. to expand on a search result). Returns the whole section by default; when a section is too large for one tool result, read it in slices with offset and length. Each section comes back with its total_length and, when content remains, the next_offset to resume from.",
	}, s.handleFetchSections)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_collections",
		Description: "List the collections available in the workspace.",
	}, s.handleListCollections)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_documents",
		Description: "List indexed documents, optionally restricted to a collection or source pattern. Returns one page at a time: total is the number of matching documents, page walks through them.",
	}, s.handleListDocuments)

	// Only advertise image retrieval when there is a store to serve from,
	// mirroring how iterative/grounding are conditioned on their configuration.
	if s.rt.Codex.Blobs() != nil {
		s.registerImageAccess()
	}
}

// registerImageAccess exposes the stored images two ways: a tool, which every
// MCP client supports, and a resource template, which is the idiomatic form for
// the clients that can dereference URIs on their own.
func (s *Server) registerImageAccess() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "fetch_image",
		Description: "Fetch an image referenced in a section content by its amoxtli://images/<hash> URI. Returns the image itself, so it can be looked at rather than only read about.",
	}, s.handleFetchImage)

	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "image",
		Title:       "Indexed image",
		Description: "An image referenced by an indexed document, addressed by the hash of its content.",
		URITemplate: blob.URIScheme + "://" + blob.URIHost + "/{hash}",
	}, s.handleReadImageResource)
}

// handleFetchImage returns the stored image as an MCP image block.
func (s *Server) handleFetchImage(ctx context.Context, _ *mcp.CallToolRequest, in fetchImageInput) (*mcp.CallToolResult, any, error) {
	data, info, err := s.readImage(ctx, in.URI)
	if err != nil {
		return nil, nil, err
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{Data: data, MIMEType: info.MimeType},
		},
	}, nil, nil
}

// handleReadImageResource serves the same bytes through the resource API.
func (s *Server) handleReadImageResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := req.Params.URI

	data, info, err := s.readImage(ctx, uri)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, mcp.ResourceNotFoundError(uri)
		}

		return nil, err
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: uri, MIMEType: info.MimeType, Blob: data},
		},
	}, nil
}

// readImage resolves a reference to the stored bytes. It refuses to serve
// anything that is not an image: the store is addressed by content hash, and
// a caller must not be able to turn it into a generic file-exfiltration
// endpoint.
func (s *Server) readImage(ctx context.Context, uri string) ([]byte, *blob.Info, error) {
	blobs := s.rt.Codex.Blobs()
	if blobs == nil {
		return nil, nil, errors.New("no image store is configured for this workspace")
	}

	hash, ok := blob.ParseURI(uri)
	if !ok {
		return nil, nil, errors.Errorf("malformed image reference %q (expected %s://%s/<hash>)", uri, blob.URIScheme, blob.URIHost)
	}

	data, info, err := blobs.Get(ctx, hash)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, nil, errors.Wrapf(err, "no image stored for %q", uri)
		}

		return nil, nil, errors.WithStack(err)
	}

	if !strings.HasPrefix(info.MimeType, "image/") {
		return nil, nil, errors.Errorf("blob %q is not an image (%s)", hash, info.MimeType)
	}

	return data, info, nil
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

		// An explicit request wins over the server-side default, including a
		// negative value, which asks for every section.
		maxSections := s.maxSectionsPerResult
		if in.MaxSectionsPerResult != 0 {
			maxSections = max(in.MaxSectionsPerResult, 0)
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

			out.Results, err = s.renderResults(ctx, result.Results, maxSections)
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

			out.Results, err = s.renderResults(ctx, page.Results, maxSections)
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

	out := fetchSectionsOutput{Sections: map[string]sectionContent{}}
	for id, section := range sections {
		content, err := section.Content()
		if err != nil {
			return nil, fetchSectionsOutput{}, errors.WithStack(err)
		}
		out.Sections[string(id)] = sliceContent(string(content), in.Offset, in.Length)
	}

	return nil, out, nil
}

// sliceContent cuts the requested range out of a section's content. It counts
// in characters rather than bytes: an offset landing in the middle of a
// multi-byte rune would hand back mojibake, and an agent paging through a
// document has no way to know where the rune boundaries are.
//
// Out of range requests are clamped instead of rejected: a caller resuming from
// a stale next_offset gets an empty slice and the real total length, which is
// enough to correct itself.
func sliceContent(content string, offset, length int) sectionContent {
	if length <= 0 {
		// No length asked for means "to the end of the section".
		length = len([]rune(content))
	}

	return takeContent(content, offset, length)
}

// takeContent is sliceContent with a length taken literally, zero included —
// the form a budget needs, where a section granted nothing must come back
// empty rather than whole.
func takeContent(content string, offset, length int) sectionContent {
	runes := []rune(content)
	total := len(runes)

	start := min(max(offset, 0), total)
	end := min(start+max(length, 0), total)

	sliced := sectionContent{
		Content:     string(runes[start:end]),
		Offset:      start,
		Length:      end - start,
		TotalLength: total,
	}
	if end < total {
		sliced.NextOffset = end
	}

	return sliced
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
	// A short page by default: documents carry their metadata, and fifty of
	// them overshoot the size limit a client puts on a tool result — a listing
	// cut at the far end loses its tail without saying so, where a page tells
	// the agent there is more by way of total.
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}

	page := max(in.Page, 0)

	query := ingest.QueryDocumentsOptions{HeaderOnly: true, Limit: &limit, Page: &page}
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
// bestSections returns at most maxSections of a result's sections, the best
// scoring ones first when the backend exposed per-section scores, otherwise the
// leading ones — index order is already relevance order for the backends that
// do not score individually.
//
// The original order is restored afterwards: sections of one document read as a
// sequence, and handing them back shuffled by score would make an excerpt
// harder to follow than it needs to be.
func bestSections(r *index.SearchResult, maxSections int) []model.SectionID {
	if maxSections <= 0 || len(r.Sections) <= maxSections {
		return r.Sections
	}

	if len(r.SectionScores) == 0 {
		return r.Sections[:maxSections]
	}

	ranked := slices.Clone(r.Sections)
	slices.SortStableFunc(ranked, func(a, b model.SectionID) int {
		// Missing scores sort last rather than first: an unscored section is
		// not evidence of relevance.
		return cmp.Compare(r.SectionScores[b], r.SectionScores[a])
	})
	ranked = ranked[:maxSections]

	kept := make([]model.SectionID, 0, maxSections)
	for _, id := range r.Sections {
		if slices.Contains(ranked, id) {
			kept = append(kept, id)
		}
	}

	return kept
}

// shareBudget hands out a budget of characters between sections of the given
// lengths, taken in the order they are to be rendered — best scoring documents
// first — and returns what each one may keep.
//
// Sections are served whole, in that order, until the budget runs out; what
// follows comes back empty, announced by its total_length. Splitting the budget
// evenly instead would look fairer but serves nobody: fifteen sections sharing
// ten thousand characters get six hundred each, which is the top of a section
// and not the passage that matched it. An agent can ask for a section it was
// told about; it cannot ask for the part of a section it does not know was cut.
func shareBudget(lengths []int, budget int) []int {
	allowed := make([]int, len(lengths))
	if budget <= 0 {
		copy(allowed, lengths)
		return allowed
	}

	remaining := budget
	for i, length := range lengths {
		allowed[i] = min(length, remaining)
		remaining -= allowed[i]
	}

	return allowed
}

func (s *Server) renderResults(ctx context.Context, results []*index.SearchResult, maxSections int) ([]documentResult, error) {
	// Trim before fetching: the sections left out are neither read from the
	// store nor rendered, so the bound saves the work as well as the bytes.
	kept := make([][]model.SectionID, 0, len(results))
	ids := []model.SectionID{}
	for _, r := range results {
		sections := bestSections(r, maxSections)
		kept = append(kept, sections)
		ids = append(ids, sections...)
	}

	sections, err := s.rt.Codex.GetSectionsByIDs(ctx, ids)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Read every kept section before rendering any of it: the budget is shared
	// out over the whole response, so how much of the first section travels
	// depends on the size of the last one.
	contents := make(map[model.SectionID]string, len(sections))
	lengths := make([]int, 0, len(ids))
	for _, id := range ids {
		section, ok := sections[id]
		if !ok {
			continue
		}
		content, err := section.Content()
		if err != nil {
			return nil, errors.WithStack(err)
		}
		contents[id] = string(content)
		lengths = append(lengths, len([]rune(contents[id])))
	}

	allowed := shareBudget(lengths, s.maxContentChars)
	next := 0

	rendered := make([]documentResult, 0, len(results))
	for i, r := range results {
		doc := documentResult{Score: r.Score, Sections: make([]sectionResult, 0, len(kept[i]))}
		if r.Source != nil {
			doc.Source = r.Source.String()
		}
		// Tell the agent what it is not seeing, so a partial view never passes
		// for the whole document: it can widen the bound or fetch the rest by
		// ID rather than conclude from an excerpt.
		if omitted := len(r.Sections) - len(kept[i]); omitted > 0 {
			doc.OmittedSections = omitted
		}

		for _, id := range kept[i] {
			section := sectionResult{ID: string(id)}
			if s, ok := sections[id]; ok {
				sliced := takeContent(contents[id], 0, allowed[next])
				next++

				section.Content = sliced.Content
				// Report the cut only when there was one: a section returned
				// whole must not look like an excerpt, and one cut short must
				// not pass for the whole section. total_length is what marks a
				// partial section, next_offset being legitimately 0 for a
				// section the budget could not afford at all.
				if sliced.Length < sliced.TotalLength {
					section.TotalLength = sliced.TotalLength
					section.NextOffset = sliced.NextOffset
				}

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
