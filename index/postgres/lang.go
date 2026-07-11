package postgres

import "github.com/abadojack/whatlanggo"

// textSearchConfigs maps ISO 639-1 language codes to the text search
// configurations shipped with PostgreSQL 13+.
var textSearchConfigs = map[string]string{
	"ar": "arabic",
	"da": "danish",
	"nl": "dutch",
	"en": "english",
	"fi": "finnish",
	"fr": "french",
	"de": "german",
	"el": "greek",
	"hu": "hungarian",
	"id": "indonesian",
	"it": "italian",
	"ga": "irish",
	"lt": "lithuanian",
	"ne": "nepali",
	"nb": "norwegian",
	"no": "norwegian",
	"pt": "portuguese",
	"ro": "romanian",
	"ru": "russian",
	"es": "spanish",
	"sv": "swedish",
	"ta": "tamil",
	"tr": "turkish",
}

// detectTextSearchConfig returns the regconfig matching the detected language
// of the given text, or fallback when detection is inconclusive. Content and
// queries are always indexed/matched under the 'simple' config as well, so a
// wrong guess only degrades stemming, never recall.
func detectTextSearchConfig(text string, fallback string) string {
	if text == "" {
		return fallback
	}

	info := whatlanggo.Detect(text)
	if !info.IsReliable() {
		return fallback
	}

	config, exists := textSearchConfigs[info.Lang.Iso6391()]
	if !exists {
		return fallback
	}

	return config
}
