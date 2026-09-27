package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Ankitwasnik/tenant-service/internal/domain"
)

// maxBodyBytes bounds a request body. The largest valid body is a create:
// a 28-character slug and a 200-character name.
const maxBodyBytes = 64 << 10

// decodeJSON reads the request body as exactly one JSON object into dst.
// Unknown fields, malformed or trailing JSON, wrong types and oversized
// bodies are validation errors that name the offending field where there is
// one, so the client can fix the request without guessing.
func decodeJSON(c *gin.Context, dst any) error {
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// Anything after the first value, other than whitespace, is rejected.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return bodyError("must contain a single JSON object")
	}
	return nil
}

func decodeError(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError

	switch {
	case errors.Is(err, io.EOF):
		return bodyError("must be a JSON object")
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		return bodyError("is not valid JSON")
	case errors.As(err, &typeErr):
		if typeErr.Field == "" { // the body itself, e.g. an array instead of an object
			return bodyError("must be a JSON object")
		}
		return fieldError(typeErr.Field, fmt.Sprintf("must be a %s", jsonType(typeErr.Type.Kind().String())))
	case errors.As(err, &tooLarge):
		return bodyError(fmt.Sprintf("must be at most %d bytes", tooLarge.Limit))
	}

	// encoding/json has no typed error for unknown fields; its message is
	// `json: unknown field "name"`.
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fieldError(strings.Trim(field, `"`), "is not a known field")
	}
	return bodyError("is not valid JSON")
}

func bodyError(msg string) error {
	return fieldError("body", msg)
}

func fieldError(field, msg string) error {
	fe := domain.FieldErrors{}
	fe.Add(field, msg)
	return fe.Err()
}

// jsonType names a Go kind the way a JSON client thinks of it.
func jsonType(kind string) string {
	switch {
	case strings.HasPrefix(kind, "int"), strings.HasPrefix(kind, "uint"), strings.HasPrefix(kind, "float"):
		return "number"
	case kind == "bool":
		return "boolean"
	case kind == "slice", kind == "array":
		return "array"
	case kind == "map", kind == "struct":
		return "object"
	}
	return kind
}
