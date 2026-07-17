package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type physicalRow struct {
	SchemaName      string `json:"schema_name"`
	TableName       string `json:"table_name"`
	ColumnOrder     string `json:"column_order"`
	ColumnName      string `json:"column_name"`
	DataType        string `json:"data_type"`
	LengthPrecision string `json:"length_precision"`
	NullConstraint  string `json:"null_constraint"`
	IsPK            string `json:"is_pk"`
	IsFK            string `json:"is_fk"`
	Description     string `json:"description"`
	Version         int    `json:"version"`
}

type logicalRow struct {
	SchemaName      string `json:"schema_name"`
	EntityNameEN    string `json:"entity_name_en"`
	EntityNameKO    string `json:"entity_name_ko"`
	EntityOrder     string `json:"entity_order"`
	AttributeNameKO string `json:"attribute_name_ko"`
	AttributeNameEN string `json:"attribute_name_en"`
	DataType        string `json:"data_type"`
	LengthPrecision string `json:"length_precision"`
	IsPK            string `json:"is_pk"`
	IsFK            string `json:"is_fk"`
	Description     string `json:"description"`
	Note            string `json:"note"`
	Version         int    `json:"version"`
}

type codeDictRow struct {
	SchemaName         string `json:"schema_name"`
	TableName          string `json:"table_name"`
	ColumnName         string `json:"column_name"`
	CommonDivisionCode string `json:"common_division_code"`
	CodeDictText       string `json:"code_dict_txt"`
}

type relationRow struct {
	ID              any    `json:"id"`
	BaseSchema      string `json:"base_schema"`
	BaseTable       string `json:"base_table"`
	BaseColumn      string `json:"base_column"`
	ReferenceSchema string `json:"reference_schema"`
	ReferenceTable  string `json:"reference_table"`
	ReferenceColumn string `json:"reference_column"`
	Cardinality     string `json:"cardinality"`
	JoinType        string `json:"join_type"`
	ProvisionType   string `json:"provision_type"`
	Description     string `json:"description"`
	MetaVersion     int    `json:"meta_version"`
}

type indexRow struct {
	SchemaName  string `json:"schema_name"`
	TableName   string `json:"table_name"`
	IndexName   string `json:"index_name"`
	ColumnName  string `json:"column_name"`
	Seq         int    `json:"seq"`
	IndexType   string `json:"index_type"`
	Description string `json:"description"`
}

func Load(dataDir string) (*Catalog, error) {
	c := &Catalog{
		DataDir:   dataDir,
		LoadedAt:  time.Now(),
		Tables:    map[string]*Table{},
		ByName:    map[string][]*Table{},
		Adjacency: map[string][]JoinEdge{},
	}

	if err := c.loadPhysical(filepath.Join(dataDir, "meta_physical_models.json")); err != nil {
		return nil, err
	}
	if err := c.loadLogical(filepath.Join(dataDir, "meta_logical_models.json")); err != nil {
		return nil, err
	}
	if len(c.Tables) == 0 {
		return nil, fmt.Errorf("catalog compiled zero tables from %s; check meta_physical_models.json and meta_logical_models.json", dataDir)
	}
	c.noteIssue("meta_code_dict.json", c.loadCodeDict(filepath.Join(dataDir, "meta_code_dict.json")))
	c.noteIssue("topology_indexes.json", c.loadIndexes(filepath.Join(dataDir, "topology_indexes.json")))
	c.noteIssue("topology_relations.json", c.loadRelations(filepath.Join(dataDir, "topology_relations.json")))
	c.noteIssue("sql_datasets.json", readJSON(filepath.Join(dataDir, "sql_datasets.json"), &c.Samples))
	c.noteIssue("meta_subject_areas.json", readJSON(filepath.Join(dataDir, "meta_subject_areas.json"), &c.Subjects))
	c.noteIssue("prompts.json", readJSON(filepath.Join(dataDir, "prompts.json"), &c.Prompts))
	c.noteIssue("databases.json", readJSON(filepath.Join(dataDir, "databases.json"), &c.Databases))

	c.Dialect = "postgres"
	for _, db := range c.Databases {
		if v := strings.ToLower(strings.TrimSpace(db.DBMS)); v != "" {
			c.Dialect = v
			break
		}
	}

	var issues []LoadIssue
	c.Glossary, issues = loadGlossary(dataDir)
	c.Issues = append(c.Issues, issues...)
	c.Metrics, issues = loadMetrics(dataDir)
	c.Issues = append(c.Issues, issues...)
	c.Overrides, issues = loadOverrides(dataDir)
	c.Issues = append(c.Issues, issues...)
	c.Patterns, issues = loadPatterns(dataDir)
	c.Issues = append(c.Issues, issues...)

	c.finalize()
	c.applyOverrides()
	c.Patterns = dialectizePatterns(c.Patterns, c.Dialect)
	c.loadColumnStats(dataDir)
	for _, t := range c.Tables {
		t.SearchText = tableSearchText(t)
	}
	c.validateRelations()
	c.validateMetrics()
	c.loadFeedback(dataDir)
	c.loadLearnedRules(dataDir)
	return c, nil
}

func (c *Catalog) noteIssue(source string, err error) {
	if err == nil {
		return
	}
	c.Issues = append(c.Issues, LoadIssue{Level: "warning", Source: source, Message: err.Error()})
}

func (c *Catalog) loadPhysical(path string) error {
	var rows []physicalRow
	if err := readJSON(path, &rows); err != nil {
		return fmt.Errorf("load physical models: %w", err)
	}
	for _, row := range rows {
		schema := cleanIdent(row.SchemaName)
		tableName := cleanIdent(row.TableName)
		colName := cleanIdent(row.ColumnName)
		if schema == "" || tableName == "" || colName == "" {
			continue
		}
		t := c.ensureTable(schema, tableName)
		if t.Description == "" {
			t.Description = cleanText(row.Description)
		}
		if row.Version > t.Version {
			t.Version = row.Version
		}
		col := t.ensureColumn(colName)
		col.DataType = strings.ToUpper(cleanText(row.DataType))
		col.LengthPrecision = cleanText(row.LengthPrecision)
		col.Nullable = cleanText(row.NullConstraint)
		col.IsPK = yes(row.IsPK)
		col.IsFK = yes(row.IsFK)
		col.Order = atoi(row.ColumnOrder)
		if col.Description == "" {
			col.Description = cleanText(row.Description)
		}
	}
	return nil
}

func (c *Catalog) loadLogical(path string) error {
	var rows []logicalRow
	if err := readJSON(path, &rows); err != nil {
		return fmt.Errorf("load logical models: %w", err)
	}
	for _, row := range rows {
		schema := cleanIdent(row.SchemaName)
		tableName := cleanIdent(row.EntityNameEN)
		colName := cleanIdent(row.AttributeNameEN)
		if schema == "" || tableName == "" || colName == "" {
			continue
		}
		t := c.ensureTable(schema, tableName)
		if v := cleanText(row.EntityNameKO); v != "" {
			t.LogicalName = v
		}
		if row.Version > t.Version {
			t.Version = row.Version
		}
		col := t.ensureColumn(colName)
		if v := cleanText(row.AttributeNameKO); v != "" {
			col.LogicalName = v
		}
		if v := cleanText(row.Description); v != "" {
			col.Description = v
		}
		if v := strings.ToUpper(cleanText(row.DataType)); v != "" && col.DataType == "" {
			col.DataType = v
		}
		if v := cleanText(row.LengthPrecision); v != "" && col.LengthPrecision == "" {
			col.LengthPrecision = v
		}
		if yes(row.IsPK) {
			col.IsPK = true
		}
		if yes(row.IsFK) {
			col.IsFK = true
		}
		if v := cleanText(row.Note); v != "" {
			col.Note = v
		}
		if n := atoi(row.EntityOrder); n > 0 {
			col.Order = n
		}
	}
	return nil
}

func (c *Catalog) loadCodeDict(path string) error {
	var rows []codeDictRow
	if err := readJSON(path, &rows); err != nil {
		return err
	}
	for _, row := range rows {
		t, ok := c.ResolveTable(cleanIdent(row.SchemaName) + "." + cleanIdent(row.TableName))
		if !ok {
			continue
		}
		col := t.ColumnMap[cleanIdent(row.ColumnName)]
		if col == nil {
			continue
		}
		col.CommonCode = cleanText(row.CommonDivisionCode)
		col.CodeDict = cleanText(row.CodeDictText)
	}
	return nil
}

func (c *Catalog) loadIndexes(path string) error {
	var rows []indexRow
	if err := readJSON(path, &rows); err != nil {
		return err
	}
	for _, row := range rows {
		schema := cleanIdent(row.SchemaName)
		tableName := cleanIdent(row.TableName)
		colName := cleanIdent(row.ColumnName)
		t, ok := c.ResolveTable(schema + "." + tableName)
		if !ok {
			continue
		}
		idx := IndexDef{
			Schema:      schema,
			Table:       tableName,
			IndexName:   cleanIdent(row.IndexName),
			ColumnName:  colName,
			Seq:         row.Seq,
			IndexType:   cleanText(row.IndexType),
			Description: cleanText(row.Description),
		}
		t.Indexes = append(t.Indexes, idx)
		if col := t.ColumnMap[colName]; col != nil {
			col.Indexed = true
		}
	}
	return nil
}

func (c *Catalog) loadRelations(path string) error {
	var rows []relationRow
	if err := readJSON(path, &rows); err != nil {
		return err
	}
	for _, row := range rows {
		r := Relation{
			ID:              row.ID,
			BaseSchema:      cleanIdent(row.BaseSchema),
			BaseTable:       cleanIdent(row.BaseTable),
			BaseColumn:      cleanColumnList(row.BaseColumn),
			ReferenceSchema: cleanIdent(row.ReferenceSchema),
			ReferenceTable:  cleanIdent(row.ReferenceTable),
			ReferenceColumn: cleanColumnList(row.ReferenceColumn),
			Cardinality:     cleanText(row.Cardinality),
			JoinType:        strings.ToUpper(cleanText(row.JoinType)),
			ProvisionType:   cleanText(row.ProvisionType),
			Description:     cleanText(row.Description),
			MetaVersion:     row.MetaVersion,
		}
		if r.BaseSchema == "" || r.BaseTable == "" || r.ReferenceSchema == "" || r.ReferenceTable == "" {
			continue
		}
		switch strings.ToUpper(r.ProvisionType) {
		case "MANUAL", "OPERATOR":
			r.Confidence = 0.9
			r.Preferred = true
		default:
			r.Confidence = 0.6
		}
		if r.Description == "" && r.Confidence > 0.6 {
			r.Confidence = 0.75
		}
		c.Relations = append(c.Relations, r)
		from := tableKey(r.BaseSchema, r.BaseTable)
		to := tableKey(r.ReferenceSchema, r.ReferenceTable)
		c.Adjacency[from] = append(c.Adjacency[from], JoinEdge{From: from, To: to, Relation: r})
		c.Adjacency[to] = append(c.Adjacency[to], JoinEdge{From: to, To: from, Relation: r, Reversed: true})
	}
	return nil
}

func (c *Catalog) finalize() {
	c.ByName = map[string][]*Table{}
	for _, t := range c.Tables {
		sort.SliceStable(t.Columns, func(i, j int) bool {
			if t.Columns[i].Order == t.Columns[j].Order {
				return t.Columns[i].Name < t.Columns[j].Name
			}
			if t.Columns[i].Order == 0 {
				return false
			}
			if t.Columns[j].Order == 0 {
				return true
			}
			return t.Columns[i].Order < t.Columns[j].Order
		})
		t.PrimaryKeys = nil
		t.ForeignKeys = nil
		for _, col := range t.Columns {
			if col.IsPK {
				t.PrimaryKeys = append(t.PrimaryKeys, col.Name)
			}
			if col.IsFK {
				t.ForeignKeys = append(t.ForeignKeys, col.Name)
			}
		}
		var wellKnown []string
		if c.Overrides != nil {
			wellKnown = c.Overrides.WellKnownDateColumns
		}
		for _, col := range t.Columns {
			if col.SemanticType == "" {
				col.SemanticType = inferSemanticType(col, wellKnown)
			}
		}
		if t.Domain == "" {
			t.Domain = inferDomain(t)
		}
		if t.Grain == "" {
			t.Grain = c.inferGrain(t)
		}
		t.SearchText = tableSearchText(t)
		c.ByName[t.Name] = append(c.ByName[t.Name], t)
	}
}

var descTagRE = regexp.MustCompile(`\[([^\[\]]+)\]`)
var tableSuffixRE = regexp.MustCompile(`\d([A-Z]+)$`)

// inferDomain derives a business-domain label from description tags like
// "[운영][카드]" or from the middle segment of the Korean logical name
// ("IA_카드계좌실적_D" -> "카드계좌실적").
func inferDomain(t *Table) string {
	tags := []string{}
	for _, m := range descTagRE.FindAllStringSubmatch(t.Description, -1) {
		tags = append(tags, m[1])
	}
	if len(tags) > 0 {
		return strings.Join(unique(tags), "/")
	}
	parts := strings.Split(t.LogicalName, "_")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// inferGrain maps the table-name suffix letter to the subject-area code name
// (e.g. TBIA70D -> D -> "상세 (Detail)").
func (c *Catalog) inferGrain(t *Table) string {
	m := tableSuffixRE.FindStringSubmatch(t.Name)
	if len(m) != 2 {
		return ""
	}
	code := m[1]
	for _, s := range c.Subjects {
		if strings.EqualFold(strings.TrimSpace(s.Code), code) {
			return s.CodeName
		}
	}
	return ""
}

func (c *Catalog) ensureTable(schema, name string) *Table {
	key := tableKey(schema, name)
	t := c.Tables[key]
	if t == nil {
		t = &Table{
			Schema:    schema,
			Name:      name,
			FQN:       key,
			ColumnMap: map[string]*Column{},
		}
		c.Tables[key] = t
	}
	return t
}

func (t *Table) ensureColumn(name string) *Column {
	col := t.ColumnMap[name]
	if col == nil {
		col = &Column{Name: name}
		t.ColumnMap[name] = col
		t.Columns = append(t.Columns, col)
	}
	return col
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func tableSearchText(t *Table) string {
	var b strings.Builder
	write := func(s string) {
		if s != "" {
			b.WriteByte(' ')
			b.WriteString(strings.ToLower(s))
		}
	}
	write(t.Schema)
	write(t.Name)
	write(t.FQN)
	write(t.LogicalName)
	write(t.Description)
	write(t.Domain)
	write(t.Grain)
	for _, col := range t.Columns {
		write(col.Name)
		write(col.LogicalName)
		write(col.Description)
		write(col.CodeDict)
		write(col.CommonCode)
		for _, s := range col.Synonyms {
			write(s)
		}
		for _, s := range col.SampleValues {
			write(s)
		}
	}
	return b.String()
}

func cleanIdent(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			return -1
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.Trim(s, "\"`[]")
	return strings.ToUpper(s)
}

func cleanColumnList(s string) string {
	parts := splitColumns(s)
	return strings.Join(parts, ", ")
}

func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			return -1
		}
		if r == '\r' || r == '\n' || r == '\t' || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func yes(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "Y") || strings.EqualFold(strings.TrimSpace(s), "YES") || strings.TrimSpace(s) == "1"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func tableKey(schema, table string) string {
	return cleanIdent(schema) + "." + cleanIdent(table)
}

func splitColumns(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = cleanIdent(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
