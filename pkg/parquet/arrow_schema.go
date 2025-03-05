package parquet

import (
	"github.com/apache/arrow/go/v15/parquet/schema"
)

// Schema is our minimal abstraction for a Parquet schema.
type Schema struct {
	node *schema.Schema
}

// NewSchema wraps an Arrow parquet schema into our Schema type.
func NewSchema(node *schema.Schema) *Schema {
	return &Schema{node: node}
}

// Node returns the underlying Arrow schema.
// This allows our code to extract metadata if needed.
func (s *Schema) Node() *schema.Schema {
	return s.node
}
