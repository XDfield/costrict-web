package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Vector represents a pgvector vector type
type Vector []float64

// Value implements driver.Valuer for database storage
func (v Vector) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	return v.String(), nil
}

// Scan implements sql.Scanner for database retrieval
func (v *Vector) Scan(src interface{}) error {
	if src == nil {
		*v = nil
		return nil
	}

	switch src := src.(type) {
	case string:
		return v.parseString(src)
	case []byte:
		return v.parseString(string(src))
	default:
		return fmt.Errorf("cannot scan %T into Vector", src)
	}
}

// String returns the vector in pgvector format
func (v Vector) String() string {
	if v == nil || len(v) == 0 {
		return "[]"
	}

	parts := make([]string, len(v))
	for i, val := range v {
		parts[i] = strconv.FormatFloat(val, 'f', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// parseString parses a pgvector string format
func (v *Vector) parseString(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" || s == "NULL" {
		*v = nil
		return nil
	}

	// Remove brackets
	s = strings.Trim(s, "[]")
	if s == "" {
		*v = nil
		return nil
	}

	// Split by comma
	parts := strings.Split(s, ",")
	result := make(Vector, len(parts))
	for i, part := range parts {
		val, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return fmt.Errorf("failed to parse vector element: %w", err)
		}
		result[i] = val
	}

	*v = result
	return nil
}

// MarshalJSON implements json.Marshaler
func (v Vector) MarshalJSON() ([]byte, error) {
	if v == nil {
		return json.Marshal(nil)
	}
	return json.Marshal([]float64(v))
}

// UnmarshalJSON implements json.Unmarshaler
func (v *Vector) UnmarshalJSON(data []byte) error {
	var slice []float64
	if err := json.Unmarshal(data, &slice); err != nil {
		return err
	}
	*v = Vector(slice)
	return nil
}

// CosineSimilarity calculates cosine similarity between two vectors using SQL
// Returns a raw SQL expression for use in queries
func CosineSimilarity(column string, vec Vector) string {
	return fmt.Sprintf("1 - (%s <=> '%s')", column, vec.String())
}

// L2Distance calculates L2 distance between two vectors using SQL
// Returns a raw SQL expression for use in queries
func L2Distance(column string, vec Vector) string {
	return fmt.Sprintf("%s <-> '%s'", column, vec.String())
}

// InnerProduct calculates inner product between two vectors using SQL
// Returns a raw SQL expression for use in queries
func InnerProduct(column string, vec Vector) string {
	return fmt.Sprintf("%s <#> '%s'", column, vec.String())
}
