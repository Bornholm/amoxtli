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

	contentFieldMapping := bleve.NewTextFieldMapping()
	contentFieldMapping.Analyzer = AnalyzerDynamicLang
	contentFieldMapping.Store = false
	contentFieldMapping.IncludeTermVectors = true
	resourceMapping.AddFieldMappingsAt("content", contentFieldMapping)

	// source is an exact identifier queried with a TermQuery (DeleteBySource,
	// collection grouping); it must be indexed as a keyword, otherwise the URL
	// is tokenized by the language analyzer and the term never matches.
	sourceFieldMapping := bleve.NewTextFieldMapping()
	sourceFieldMapping.Analyzer = keyword.Name
	sourceFieldMapping.Store = true
	sourceFieldMapping.IncludeTermVectors = true
	resourceMapping.AddFieldMappingsAt("source", sourceFieldMapping)

	// collections holds exact identifiers matched with a TermQuery; keyword
	// indexing keeps each id as a single, verbatim term.
	collectionsFieldMapping := bleve.NewTextFieldMapping()
	collectionsFieldMapping.Analyzer = keyword.Name
	collectionsFieldMapping.Store = false
	collectionsFieldMapping.IncludeTermVectors = true
	resourceMapping.AddFieldMappingsAt("collections", collectionsFieldMapping)

	mapping.AddDocumentMapping("resource", resourceMapping)

	return mapping
}
