package api

import (
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
	"github.com/Ankitwasnik/tenant-service/internal/store"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// parsePage reads ?limit= and ?cursor= (DESIGN.md §7). A malformed value is a
// validation_error naming the parameter; fe collects it, so the caller can
// report every bad parameter at once.
func parsePage(c *gin.Context, fe domain.FieldErrors) store.Page {
	p := store.Page{Limit: defaultPageLimit}

	if raw, ok := c.GetQuery("limit"); ok {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxPageLimit {
			fe.Add("limit", fmt.Sprintf("must be an integer from 1 to %d", maxPageLimit))
		} else {
			p.Limit = limit
		}
	}
	p.Cursor = parseQueryUUID(c, "cursor", "must be a next_cursor value from a previous page", fe)
	return p
}

// parseQueryUUID reads an optional UUID query parameter: nil if it is absent,
// and a field error if it is present but malformed.
func parseQueryUUID(c *gin.Context, name, msg string, fe domain.FieldErrors) *uuid.UUID {
	raw, ok := c.GetQuery(name)
	if !ok {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		fe.Add(name, msg)
		return nil
	}
	return &id
}

// pathID parses the :id path parameter. A malformed id can't name any
// resource, so it gets the same not-found error as an unknown one
// (DESIGN.md §7): notFound builds it from the id.
func pathID(c *gin.Context, notFound func(uuid.UUID) *domain.Error) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		e := notFound(uuid.Nil)
		e.Message = fmt.Sprintf("%q is not a known id", c.Param("id"))
		respondError(c, e)
		return uuid.Nil, false
	}
	return id, true
}
