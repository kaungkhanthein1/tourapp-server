package places

import (
	"strings"

	"travel-app-backend/internal/pkg/external"
)

// MinCuratedProseLen is the shortest description that counts as real
// wiki prose. Location-line fallbacks ("Paris, France") stay below it.
const MinCuratedProseLen = 80

// categoryForbiddenTokens rejects provider photos whose file title
// contradicts the card's chip. Conservative on purpose: only strong
// subject mismatches, whole-word matches against the normalized title.
var categoryForbiddenTokens = map[string][]string{
	"Beaches":      {"interior", "building", "statue", "museum", "airport", "station", "street", "market"},
	"Hills":        {"interior", "statue", "museum", "airport", "station", "street", "market", "bridge"},
	"Parks":        {"interior", "airport", "station", "market"},
	"Architecture": {"underwater", "waterfall", "cave", "volcano", "glacier", "forest", "jungle", "desert", "dune", "reef", "lagoon"},
	"Cities":       {"underwater", "waterfall", "cave", "glacier", "reef"},
	"Foods":        {"interior", "station", "airport"},
}

// titleTokenizer splits file titles into whole words for token matching.
var titleTokenizer = strings.NewReplacer("_", " ", "-", " ", ".", " ", "(", " ", ")", " ")

// CuratedPlaceMedia reports whether a resolved provider row passes the
// feed's media bar: subject-appropriate Wikimedia file title (no maps,
// diagrams, postcard-era scans, or chip mismatches) and, except for the
// structural Cities rows, real wiki prose. Proximity and resolution are
// checked by the bootstrap curation pass, which needs DB-wide context.
func CuratedPlaceMedia(category, image, description string) bool {
	if category != "Cities" && len(strings.TrimSpace(description)) < MinCuratedProseLen {
		return false
	}
	title := external.WikimediaFileTitle(image)
	if title == "" {
		// Not a Wikimedia URL: no title signal, keep the row.
		return true
	}
	if external.PhotoSubjectJunk(title) {
		return false
	}
	normalized := strings.Fields(titleTokenizer.Replace(strings.ToLower(title)))
	for _, token := range categoryForbiddenTokens[category] {
		for _, word := range normalized {
			if word == token {
				return false
			}
		}
	}
	return true
}
