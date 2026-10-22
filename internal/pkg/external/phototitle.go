package external

import (
	"net/url"
	"regexp"
	"strings"
)

// postcardYearToken matches standalone filename years 1500-1949, which
// indicate dated scans rather than modern photos.
var postcardYearToken = regexp.MustCompile(`(^|[^a-z0-9])(1[5-9]\d{2}|19[0-4]\d)([^a-z0-9]|$)`)

// commonsJunkTokens reject files that match a search but are never the
// entity's photo: autographs, diagrams, official symbols, scans. Matched
// as whole words against the normalized (de-punctuated) file title.
var commonsJunkTokens = map[string]bool{
	"signature": true, "autograph": true, "diagram": true, "drawing": true,
	"sketch": true, "engraving": true, "representation": true, "logo": true,
	"emblem": true, "flag": true, "stamp": true, "banknote": true,
	"coin": true, "chart": true, "schematic": true, "screenshot": true,
	"barcode": true, "alphabet": true, "watermark": true, "clipart": true,
	"silhouette": true, "specimen": true,
}

// normalizeTitle lowercases and de-punctuates a file title so token
// comparisons are stable ("File:Rachel_Tayza.jpg" → "file rachel tayza jpg").
func normalizeTitle(title string) string {
	n := strings.ToLower(title)
	for _, r := range []string{"_", "-", ".", ",", ":", "(", ")", "'", "’", "“", "”"} {
		n = strings.ReplaceAll(n, r, " ")
	}
	return strings.Join(strings.Fields(n), " ")
}

// curationJunkTokens extend the Commons resolver junk set with
// diagrammatic titles that are never beautiful card photos.
var curationJunkTokens = func() map[string]struct{} {
	set := map[string]struct{}{}
	for token := range commonsJunkTokens {
		set[token] = struct{}{}
	}
	for _, token := range []string{
		"map", "maps", "locator", "plan", "plans", "seal", "arms",
		"banner", "overview", "schema", "scheme",
	} {
		set[token] = struct{}{}
	}
	return set
}()

// WikimediaFileTitle extracts the underlying file name from a cached
// Wikimedia image URL (thumb or original), with spaces preserved:
//
//	.../thumb/3/39/Phad_thai_edit.jpg/800px-Phad_thai_edit.jpg -> "Phad thai edit.jpg"
//	.../commons/9/97/Tom_yum_goong.jpg                        -> "Tom yum goong.jpg"
//
// Returns "" when the URL is not a parseable Wikimedia image URL.
func WikimediaFileTitle(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Host != "upload.wikimedia.org" && parsed.Host != "thumb.wikimedia.org") {
		return ""
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return ""
	}
	last := segments[len(segments)-1]
	// Thumb URLs end in "<width>px-<name>"; the real file is the
	// preceding segment.
	if len(segments) >= 2 && thumbNameToken(last) {
		last = segments[len(segments)-2]
	}
	name, err := url.PathUnescape(last)
	if err != nil {
		return last
	}
	return name
}

func thumbNameToken(segment string) bool {
	lower := strings.ToLower(segment)
	if !strings.Contains(lower, "px-") {
		return false
	}
	index := strings.Index(lower, "px-")
	if index == 0 {
		return false
	}
	for _, ch := range lower[:index] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// PhotoSubjectJunk reports whether a Wikimedia file title looks like a
// map, logo, diagram, or dated postcard scan - never a beautiful card
// photo. Empty titles carry no signal and are not junk.
func PhotoSubjectJunk(fileTitle string) bool {
	if fileTitle == "" {
		return false
	}
	normalized := normalizeTitle(fileTitle)
	if postcardYearToken.MatchString(normalized) {
		return true
	}
	for _, token := range strings.Fields(normalized) {
		if _, junk := curationJunkTokens[token]; junk {
			return true
		}
	}
	return false
}
