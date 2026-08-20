package bleve

import (
	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/mapping"
)

func IndexMapping() *mapping.IndexMappingImpl {
	mapping := bleve.NewIndexMapping()

	mapping.TypeField = "_type"
	mapping.DefaultAnalyzer = AnalyzerDynamicLang

	resourceMapping := bleve.NewDocumentMapping()

	// Term vectors record the position of every term in the field. They
	// only serve highlighting and positional queries, and Search uses
	// neither: carrying them typically doubles the index size for nothing.
	// DocValues serve sorting and faceting, equally unused.
	contentFieldMapping := bleve.NewTextFieldMapping()
	contentFieldMapping.Analyzer = AnalyzerDynamicLang
	contentFieldMapping.Store = false
	contentFieldMapping.IncludeTermVectors = false
	contentFieldMapping.DocValues = false
	resourceMapping.AddFieldMappingsAt("content", contentFieldMapping)

	// source is an exact identifier queried with a TermQuery (DeleteBySource,
	// collection grouping); it must be indexed as a keyword, otherwise the URL
	// is tokenized by the language analyzer and the term never matches.
	sourceFieldMapping := bleve.NewTextFieldMapping()
	sourceFieldMapping.Analyzer = keyword.Name
	sourceFieldMapping.Store = true
	sourceFieldMapping.IncludeTermVectors = false
	sourceFieldMapping.DocValues = false
	resourceMapping.AddFieldMappingsAt("source", sourceFieldMapping)

	// collections holds exact identifiers matched with a TermQuery; keyword
	// indexing keeps each id as a single, verbatim term.
	collectionsFieldMapping := bleve.NewTextFieldMapping()
	collectionsFieldMapping.Analyzer = keyword.Name
	collectionsFieldMapping.Store = false
	collectionsFieldMapping.IncludeTermVectors = false
	collectionsFieldMapping.DocValues = false
	resourceMapping.AddFieldMappingsAt("collections", collectionsFieldMapping)

	mapping.AddDocumentMapping("resource", resourceMapping)

	return mapping
}
