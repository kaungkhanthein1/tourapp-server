package pagination

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// Defaults for list endpoints.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Page is the standardized limit/offset pair for list queries.
type Page struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Parse reads ?limit=&offset= with clamping: limit in [1,100] (default 20), offset >= 0.
func Parse(c *gin.Context) Page {
	limit := DefaultLimit
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	offset := 0
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}

	return Page{Limit: limit, Offset: offset}
}

// Meta is the response metadata block for paginated lists.
type Meta struct {
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
	Total  int64 `json:"total"`
}

// NewMeta builds a Meta from a Page and the total row count.
func NewMeta(p Page, total int64) Meta {
	return Meta{Limit: p.Limit, Offset: p.Offset, Total: total}
}
