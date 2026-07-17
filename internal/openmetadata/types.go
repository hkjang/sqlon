package openmetadata

import "strings"

// Table mirrors the subset of the OpenMetadata Table entity jamypg consumes.
type Table struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	FullyQualifiedName string     `json:"fullyQualifiedName"`
	DisplayName        string     `json:"displayName,omitempty"`
	Description        string     `json:"description,omitempty"`
	Columns            []Column   `json:"columns,omitempty"`
	Tags               []TagLabel `json:"tags,omitempty"`
}

// Column mirrors the OpenMetadata Column type.
type Column struct {
	Name               string     `json:"name"`
	FullyQualifiedName string     `json:"fullyQualifiedName,omitempty"`
	DisplayName        string     `json:"displayName,omitempty"`
	DataType           string     `json:"dataType,omitempty"`
	Description        string     `json:"description,omitempty"`
	Tags               []TagLabel `json:"tags,omitempty"`
}

// TagLabel is one applied tag/classification (e.g. tagFQN "PII.Sensitive").
type TagLabel struct {
	TagFQN string `json:"tagFQN"`
	Source string `json:"source,omitempty"` // Classification | Glossary
}

// GlossaryTerm mirrors the OpenMetadata GlossaryTerm entity.
type GlossaryTerm struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Description string   `json:"description,omitempty"`
	Synonyms    []string `json:"synonyms,omitempty"`
}

type paging struct {
	Total int    `json:"total"`
	After string `json:"after,omitempty"`
}

type tableList struct {
	Data   []Table `json:"data"`
	Paging paging  `json:"paging"`
}

type glossaryList struct {
	Data   []GlossaryTerm `json:"data"`
	Paging paging         `json:"paging"`
}

// SchemaTable reduces an OpenMetadata 4-part FQN
// (service.database.schema.table) to jamypg's 2-part "schema.table".
// Handles quoted segments ("svc"."db"."schema"."tbl") and fewer parts.
func SchemaTable(omFQN string) string {
	parts := splitFQN(omFQN)
	n := len(parts)
	if n >= 2 {
		return parts[n-2] + "." + parts[n-1]
	}
	if n == 1 {
		return parts[0]
	}
	return ""
}

// splitFQN splits an OpenMetadata FQN on unquoted dots and strips surrounding
// double quotes from each segment.
func splitFQN(fqn string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for _, r := range fqn {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '.' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// IsPII reports whether any tag marks the column as sensitive personal data.
// OpenMetadata's default PII classification uses tagFQN "PII.Sensitive".
func (col Column) IsPII() bool {
	for _, t := range col.Tags {
		fq := strings.ToLower(t.TagFQN)
		if strings.HasPrefix(fq, "pii.") && strings.Contains(fq, "sensitive") && !strings.Contains(fq, "nonsensitive") {
			return true
		}
	}
	return false
}
