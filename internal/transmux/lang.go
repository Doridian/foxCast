package transmux

import "strings"

// language maps between Matroska's ISO 639-2 codes (bibliographic or
// terminology), the BCP 47 tags HLS wants, and an English display name.
type language struct {
	tag  string // BCP 47 / ISO 639-1
	iso3 string // ISO 639-2/T, for mdhd
	name string
}

var languages = []language{
	{"en", "eng", "English"}, {"fr", "fra", "French"}, {"de", "deu", "German"},
	{"es", "spa", "Spanish"}, {"it", "ita", "Italian"}, {"pt", "por", "Portuguese"},
	{"nl", "nld", "Dutch"}, {"sv", "swe", "Swedish"}, {"da", "dan", "Danish"},
	{"no", "nor", "Norwegian"}, {"nb", "nob", "Norwegian Bokmål"}, {"fi", "fin", "Finnish"},
	{"is", "isl", "Icelandic"}, {"pl", "pol", "Polish"}, {"cs", "ces", "Czech"},
	{"sk", "slk", "Slovak"}, {"hu", "hun", "Hungarian"}, {"ro", "ron", "Romanian"},
	{"bg", "bul", "Bulgarian"}, {"hr", "hrv", "Croatian"}, {"sr", "srp", "Serbian"},
	{"sl", "slv", "Slovenian"}, {"el", "ell", "Greek"}, {"tr", "tur", "Turkish"},
	{"ru", "rus", "Russian"}, {"uk", "ukr", "Ukrainian"}, {"he", "heb", "Hebrew"},
	{"ar", "ara", "Arabic"}, {"fa", "fas", "Persian"}, {"hi", "hin", "Hindi"},
	{"th", "tha", "Thai"}, {"vi", "vie", "Vietnamese"}, {"id", "ind", "Indonesian"},
	{"ms", "msa", "Malay"}, {"zh", "zho", "Chinese"}, {"ja", "jpn", "Japanese"},
	{"ko", "kor", "Korean"}, {"ca", "cat", "Catalan"}, {"eu", "eus", "Basque"},
	{"gl", "glg", "Galician"}, {"et", "est", "Estonian"}, {"lv", "lav", "Latvian"},
	{"lt", "lit", "Lithuanian"}, {"ta", "tam", "Tamil"}, {"te", "tel", "Telugu"},
}

// bibliographic ISO 639-2/B codes that differ from the /T codes above.
var bibliographic = map[string]string{
	"fre": "fra", "ger": "deu", "dut": "nld", "cze": "ces", "slo": "slk",
	"rum": "ron", "gre": "ell", "per": "fas", "chi": "zho", "ice": "isl",
	"may": "msa", "baq": "eus",
}

// lookupLanguage resolves a Matroska language (ISO 639-2 or BCP 47).
func lookupLanguage(code string) (language, bool) {
	code = strings.ToLower(code)
	primary, _, _ := strings.Cut(code, "-")
	if t, ok := bibliographic[primary]; ok {
		primary = t
	}
	for _, l := range languages {
		if l.tag == primary || l.iso3 == primary {
			return l, true
		}
	}
	return language{}, false
}

// hlsLanguage returns the LANGUAGE attribute value, or "" for undetermined.
func hlsLanguage(code string) string {
	if l, ok := lookupLanguage(code); ok {
		if strings.Contains(code, "-") {
			return code // keep region/script subtags
		}
		return l.tag
	}
	if code == "" || strings.EqualFold(code, "und") {
		return ""
	}
	return code
}

// mdhdLanguage returns an ISO 639-2/T code for the mdhd box.
func mdhdLanguage(code string) string {
	if l, ok := lookupLanguage(code); ok {
		return l.iso3
	}
	return "und"
}

// languageName returns an English display name, or "" if unknown.
func languageName(code string) string {
	if l, ok := lookupLanguage(code); ok {
		return l.name
	}
	return ""
}
