package bleve

import (
	"bytes"
	"strings"

	"github.com/abadojack/whatlanggo"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
	"github.com/blevesearch/bleve/v2/registry"

	_ "github.com/blevesearch/bleve/v2/analysis/lang/ar"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/bg"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ca"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ckb"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/cs"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/da"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/de"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/el"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/en"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/es"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/eu"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/fa"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/fi"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/fr"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ga"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/gl"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/hi"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/hu"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/hy"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/id"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/in"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/it"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/nl"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/no"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/pt"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ro"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/ru"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/sv"
	_ "github.com/blevesearch/bleve/v2/analysis/lang/tr"
)

const AnalyzerDynamicLang = "dynamiclang"

type DetectLangAppendCharFilter struct {
	defaultLang string
}

func (c *DetectLangAppendCharFilter) Filter(input []byte) []byte {
	var langs []string
	hasDefLang := false
	if len(input) > 0 {
		sInput := string(input)
		options := whatlanggo.Options{
			Blacklist: map[whatlanggo.Lang]bool{},
		}
		for i := 0; i < 8; i++ {
			info := whatlanggo.DetectWithOptions(sInput, options)
			options.Blacklist[info.Lang] = true
			lang := info.Lang.Iso6391()
			langs = append(langs, lang)
			if !hasDefLang && lang == c.defaultLang {
				hasDefLang = true
			}
			if !info.IsReliable() {
				if !hasDefLang {
					langs = append(langs, c.defaultLang)
					hasDefLang = true
				}
				break
			}
		}
	}
	if !hasDefLang {
		langs = append(langs, c.defaultLang)
	}

	input = append(input, []byte(";"+strings.Join(langs, ","))...)
	return input
}

var germanSpecialReplacer = strings.NewReplacer(
	"ä", "ae",
	"Ä", "AE",
	"ö", "oe",
	"Ö", "OE",
	"ü", "ue",
	"Ü", "UE",
	"ß", "ss",
	"ẞ", "SS",
)

type GermanSpecialTokenFilter struct {
}

func (c *GermanSpecialTokenFilter) Filter(ts analysis.TokenStream) analysis.TokenStream {
	var output bytes.Buffer
	for _, t := range ts {
		germanSpecialReplacer.WriteString(&output, string(t.Term))
		newTerm := output.Bytes()
		t.Term = make([]byte, len(newTerm))
		copy(t.Term, newTerm)
		output.Reset()
	}
	return ts
}

type DynamicLangTokenizer struct {
	cache           *registry.Cache
	defaultAnalyzer analysis.Analyzer
}

func (d *DynamicLangTokenizer) Tokenize(input []byte) analysis.TokenStream {
	beg := bytes.LastIndexByte(input, ';')
	if beg >= 0 {
		langPart := input[beg+1:]
		input = input[:beg]
		langs := bytes.Split(langPart, []byte{','})
		var ts analysis.TokenStream
		for _, lang := range langs {
			sLang := string(lang)
			analyzer, err := d.cache.AnalyzerNamed(sLang)
			if err != nil {
				continue
			}
			ts = append(ts, analyzer.Analyze(input)...)
		}
		return ts
	}
	return d.defaultAnalyzer.Analyze(input)
}

type AsciiFoldingTokenFilter struct {
	asciifoldingCharFilter analysis.CharFilter
}

func (tf *AsciiFoldingTokenFilter) Filter(ts analysis.TokenStream) analysis.TokenStream {
	for _, t := range ts {
		t.Term = tf.asciifoldingCharFilter.Filter(t.Term)
	}
	return ts
}

func init() {
	registry.RegisterCharFilter("detectlangappend", func(config map[string]interface{}, cache *registry.Cache) (analysis.CharFilter, error) {
		if dLang, ok := config["default_lang"].(string); ok {
			return &DetectLangAppendCharFilter{defaultLang: dLang}, nil
		}
		return &DetectLangAppendCharFilter{defaultLang: en.AnalyzerName}, nil
	})

	registry.RegisterTokenFilter("germanspecialtokenfilter", func(config map[string]interface{}, cache *registry.Cache) (analysis.TokenFilter, error) {
		return &GermanSpecialTokenFilter{}, nil
	})

	registry.RegisterTokenFilter("asciifoldingtokenfilter", func(config map[string]interface{}, cache *registry.Cache) (analysis.TokenFilter, error) {
		asciifoldingCharFilter, err := cache.CharFilterNamed(asciifolding.Name)
		if err != nil {
			return nil, err
		}
		return &AsciiFoldingTokenFilter{asciifoldingCharFilter: asciifoldingCharFilter}, nil
	})

	registry.RegisterTokenizer("dynamiclangtokenizer", func(config map[string]interface{}, cache *registry.Cache) (analysis.Tokenizer, error) {
		var defaultAnalyzer analysis.Analyzer

		if dLang, ok := config["default_lang"].(string); ok {
			var err error
			defaultAnalyzer, err = cache.AnalyzerNamed(dLang)
			if err != nil {
				defaultAnalyzer, _ = cache.AnalyzerNamed(en.AnalyzerName)
			}
		} else {
			defaultAnalyzer, _ = cache.AnalyzerNamed(en.AnalyzerName)
		}

		return &DynamicLangTokenizer{
			cache:           cache,
			defaultAnalyzer: defaultAnalyzer,
		}, nil
	})

	registry.RegisterAnalyzer(AnalyzerDynamicLang, func(config map[string]interface{}, cache *registry.Cache) (analysis.Analyzer, error) {
		detectLangAppendCharFilter, err := cache.CharFilterNamed("detectlangappend")
		if err != nil {
			return nil, err
		}

		dynamicLangTokenizer, err := cache.TokenizerNamed("dynamiclangtokenizer")
		if err != nil {
			return nil, err
		}

		germanSpecialTokenFilter, err := cache.TokenFilterNamed("germanspecialtokenfilter")
		if err != nil {
			return nil, err
		}

		asciiFoldingTokenFilter, err := cache.TokenFilterNamed("asciifoldingtokenfilter")
		if err != nil {
			return nil, err
		}

		rv := analysis.DefaultAnalyzer{
			CharFilters: []analysis.CharFilter{
				detectLangAppendCharFilter,
			},
			Tokenizer: dynamicLangTokenizer,
			TokenFilters: []analysis.TokenFilter{
				germanSpecialTokenFilter,
				asciiFoldingTokenFilter,
			},
		}

		return &rv, nil
	})
}
