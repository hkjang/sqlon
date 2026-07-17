package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DatasetInfo is the registry entry for one JSON dataset the MCP server
// references. The registry is the single source of truth for what each file
// is, who consumes it, and whether operators may replace or remove it — it
// powers list_datasets / get_dataset / put_dataset / remove_dataset.
type DatasetInfo struct {
	Name        string   `json:"name"`
	File        string   `json:"file"`
	Required    bool     `json:"required"`
	Editable    bool     `json:"editable"`
	Format      string   `json:"format"` // json-array | json-object | jsonl-dir
	Description string   `json:"description"`
	Schema      string   `json:"schema"`
	UsedBy      []string `json:"used_by"`
}

// DatasetRegistry enumerates every dataset the catalog loader knows about.
// put_dataset/remove_dataset refuse names outside this registry, which also
// prevents path traversal.
var DatasetRegistry = []DatasetInfo{
	{
		Name: "physical_models", File: "meta_physical_models.json", Required: true, Editable: true, Format: "json-array",
		Description: "물리 모델 원장: 스키마/테이블/컬럼 물리명, 타입, 길이, PK/FK, 설명. 카탈로그의 기준 데이터로 없으면 서버가 시작하지 않음.",
		Schema:      "[{schema_name, table_name, column_order, column_name, data_type, length_precision, null_constraint, is_pk, is_fk, description, version}]",
		UsedBy:      []string{"search_schema", "get_schema_context", "validate_sql", "get_column_stats", "build_sql_skeleton", "explain_sql"},
	},
	{
		Name: "logical_models", File: "meta_logical_models.json", Required: true, Editable: true, Format: "json-array",
		Description: "논리 모델: 엔티티/속성 한글명(논리명)과 설명. 한국어 질문을 물리 컬럼에 매핑하는 핵심 데이터.",
		Schema:      "[{schema_name, entity_name_en, entity_name_ko, attribute_name_ko, attribute_name_en, data_type, is_pk, is_fk, description, note, version}]",
		UsedBy:      []string{"search_schema", "get_schema_context", "analyze_question", "find_filter_columns"},
	},
	{
		Name: "code_dict", File: "meta_code_dict.json", Required: false, Editable: true, Format: "json-array",
		Description: "코드 사전: 코드 컬럼의 유효값과 라벨(예: '00:정상, 99:삭제'). 값 리터럴 검증과 값→컬럼 매핑에 사용.",
		Schema:      "[{schema_name, table_name, column_name, common_division_code, code_dict_txt}]",
		UsedBy:      []string{"find_filter_columns", "validate_sql(CODE_VALUE_UNKNOWN)", "search_schema", "get_column_stats"},
	},
	{
		Name: "relations", File: "topology_relations.json", Required: false, Editable: true, Format: "json-array",
		Description: "조인 그래프 엣지: 테이블 간 조인 관계(컬럼, cardinality, join type, 설명). SQL의 모든 ON 조건은 여기서 나옴.",
		Schema:      "[{base_schema, base_table, base_column, reference_schema, reference_table, reference_column, cardinality, join_type, provision_type, description}]",
		UsedBy:      []string{"get_join_paths", "validate_sql", "build_sql_skeleton", "suggest_joins", "get_schema_context"},
	},
	{
		Name: "indexes", File: "topology_indexes.json", Required: false, Editable: true, Format: "json-array",
		Description: "인덱스 정의: 실행계획 힌트와 컬럼 선택 가중치에 사용.",
		Schema:      "[{schema_name, table_name, index_name, column_name, seq, index_type, description}]",
		UsedBy:      []string{"explain_sql", "search_schema", "suggest_joins"},
	},
	{
		Name: "sql_examples", File: "sql_datasets.json", Required: false, Editable: true, Format: "json-array",
		Description: "골든 질문-SQL 예제(few-shot). intent 시그니처 매칭, 검색 부스트, 날짜 컬럼 선택, 골든셋 생성의 원천.",
		Schema:      "[{id, question, target_sql, target_domain, target_table, target_column, target_intent, target_difficulty}]",
		UsedBy:      []string{"search_examples", "search_schema", "analyze_question", "build_sql_skeleton", "jamypg-goldgen"},
	},
	{
		Name: "subject_areas", File: "meta_subject_areas.json", Required: false, Editable: true, Format: "json-array",
		Description: "주제영역/접미사 규칙: 테이블명 접미사(S=요약, D=상세 등)로 데이터 grain을 추론.",
		Schema:      "[{category, rule, code, code_name, description, keywords}]",
		UsedBy:      []string{"search_schema(grain)", "explain_sql(대용량 판단)"},
	},
	{
		Name: "prompts", File: "prompts.json", Required: false, Editable: true, Format: "json-array",
		Description: "MCP prompt 템플릿 원장 (prompts/list, prompts/get으로 노출).",
		Schema:      "[{name, role, category, content, description, is_active}]",
		UsedBy:      []string{"prompts/list", "prompts/get"},
	},
	{
		Name: "databases", File: "databases.json", Required: false, Editable: true, Format: "json-array",
		Description: "대상 DB 정의(방언 결정용). 자격증명은 암호화된 상태로 저장됨.",
		Schema:      "[{id, dbms, host(enc), port, username(enc), password(enc), name, alias}]",
		UsedBy:      []string{"validate_sql(방언)", "explain_sql"},
	},
	{
		Name: "glossary", File: "glossary.json", Required: false, Editable: true, Format: "json-object",
		Description: "업무 용어/동의어 사전. 검색·질문분해·지표조회·검증이 모두 같은 사전을 사용. 없으면 내장 기본값 사용.",
		Schema:      "{entries: [{term, synonyms[], category(entity|metric|dimension|value|time), note}]}",
		UsedBy:      []string{"search_schema", "analyze_question", "get_metric_definition", "validate_sql(expected_outputs)", "search_examples"},
	},
	{
		Name: "metrics", File: "metrics.json", Required: false, Editable: true, Format: "json-array",
		Description: "지표 사전: 업무 지표의 확정 계산식·필수 필터·grain. LLM이 계산식을 만들지 않도록 하는 1순위 근거.",
		Schema:      "[{name, business_name, aliases[], description, expression, aggregation, tables[], columns[], allowed_grains[], recommended_group_by[], required_filters[], exclusions[], null_handling, dedup_key, example_sql}]",
		UsedBy:      []string{"get_metric_definition", "search_schema", "analyze_question", "validate_sql(METRIC_MISMATCH)", "build_sql_skeleton"},
	},
	{
		Name: "overrides", File: "overrides.json", Required: false, Editable: true, Format: "json-object",
		Description: "운영자 보정: 테이블/컬럼 설명·도메인·grain, 동의어, 샘플값, PII 지정, 권장/금지 조인, 기본 필터, 방언.",
		Schema:      "{dialect, tables[], columns[], pii_columns[], forbidden_joins[], preferred_joins[], default_filters[]}",
		UsedBy:      []string{"모든 검색/검증/컨텍스트 도구", "get_join_paths", "validate_sql(PII/FORBIDDEN_JOIN)"},
	},
	{
		Name: "patterns", File: "patterns.json", Required: false, Editable: true, Format: "json-array",
		Description: "다단계 SQL 패턴 사전(2단 집계, 그룹별 top-N, 전월/전년 대비, 비율, 분포). 없으면 내장 기본값 사용.",
		Schema:      "[{name, keywords[], description, template, slots[], caution}]",
		UsedBy:      []string{"analyze_question(patterns)", "build_sql_skeleton"},
	},
	{
		Name: "column_stats", File: "column_stats.json", Required: false, Editable: true, Format: "json-array",
		Description: "컬럼 프로파일 통계: row count, null 비율, distinct, min/max, top values, 최신성. 값→컬럼 매핑과 대용량 판단에 사용.",
		Schema:      "[{schema_name, table_name, column_name, row_count, null_ratio, distinct_count, min, max, top_values[{value, ratio, label}], format_pattern, last_updated}]",
		UsedBy:      []string{"get_column_stats", "find_filter_columns", "search_schema", "explain_sql"},
	},
	{
		Name: "golden_queries", File: "golden_queries.json", Required: false, Editable: true, Format: "json-array",
		Description: "평가용 골든 쿼리셋. run_evaluation과 CI(go test)가 정확도 회귀를 측정.",
		Schema:      "[{id, question, expected_tables[], expected_columns[], expected_metrics[], expected_sql, note}]",
		UsedBy:      []string{"run_evaluation", "jamypg-eval", "go test"},
	},
	{
		Name: "learned_rules", File: "learned_rules.json", Required: false, Editable: true, Format: "json-array",
		Description: "learn_from_feedback가 승격한 학습 룰(반복 오류/테이블·컬럼 교정). 운영자가 검토·수정·삭제 가능.",
		Schema:      "[{type(recurring_error|table_correction|column_correction), code, table, column, replacement_table, replacement_column, message, hint, occurrences, updated_at}]",
		UsedBy:      []string{"validate_sql(LEARNED_*)", "search_schema(패널티)", "learn_from_feedback"},
	},
	{
		Name: "db_profiles", File: "db_profiles.json", Required: false, Editable: true, Format: "json-array",
		Description: "DB 접속 프로파일(postgres/mysql/mariadb): 접속 문자열(host:port/dbname 또는 URL), read-only 계정, 커넥션 풀·타임아웃·행 제한 정책. 비밀번호는 password_ref(env:/file:)로 참조하며 평문 저장 금지.",
		Schema:      "[{id, name, type(postgres|mysql|mariadb), driver(pgx|mysql), connect_string, username, password_ref(env:NAME|file:PATH|plain:VALUE), pool{max_open_conns, max_idle_conns, conn_max_lifetime_seconds, conn_max_idle_time_seconds}, policy{query_timeout_seconds, connection_test_timeout_seconds, default_max_rows, max_rows, max_response_bytes, denied_keywords[]}}]",
		UsedBy:      []string{"run_sql_safely(실행)", "/api/query/*", "/api/db-profiles/*", "/admin/db"},
	},
	{
		Name: "feedback", File: "feedback", Required: false, Editable: false, Format: "jsonl-dir",
		Description: "record_feedback 저장소(JSONL, 일자별). 시스템이 관리하며 put/remove 대상이 아님. 성공 SQL 재사용과 룰 승격의 원천.",
		Schema:      "feedback-YYYYMMDD.jsonl: {question, analysis, tables[], columns[], generated_sql, validation_errors, final_sql, executed, adopted, outcome, duration_ms, result_rows, failure_cause, notes}",
		UsedBy:      []string{"record_feedback", "learn_from_feedback", "search_schema", "analyze_question"},
	},
	{
		Name: "audit", File: "audit", Required: false, Editable: false, Format: "jsonl-dir",
		Description: "모든 MCP tool call 감사 로그(JSONL, 일자별). 시스템이 자동 기록하며 put/remove 대상이 아님.",
		Schema:      "audit-YYYYMMDD.jsonl: {ts, tool, arguments, duration_ms, is_error, error}",
		UsedBy:      []string{"감사/추적"},
	},
}

// FileBackedDatasets returns the editable datasets stored as single JSON
// files (name, file), i.e. everything except system-managed jsonl dirs. These
// are the datasets that can be moved into / materialized from the meta DB.
func FileBackedDatasets() []DatasetInfo {
	out := []DatasetInfo{}
	for _, d := range DatasetRegistry {
		if d.Editable && d.Format != "jsonl-dir" {
			out = append(out, d)
		}
	}
	return out
}

// DatasetFileByName maps a dataset name to its on-disk filename.
func DatasetFileByName(name string) (string, bool) {
	d, ok := findDataset(name)
	if !ok || d.Format == "jsonl-dir" {
		return "", false
	}
	return d.File, true
}

func findDataset(name string) (DatasetInfo, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, d := range DatasetRegistry {
		if d.Name == n || d.File == name {
			return d, true
		}
	}
	return DatasetInfo{}, false
}

// datasetLoadedCount reports how many entries of a dataset made it into the
// compiled catalog, so operators can spot silently-dropped rows.
func (c *Catalog) datasetLoadedCount(name string) any {
	switch name {
	case "physical_models", "logical_models":
		cols := 0
		for _, t := range c.Tables {
			cols += len(t.Columns)
		}
		return map[string]int{"tables": len(c.Tables), "columns": cols}
	case "relations":
		return len(c.Relations)
	case "sql_examples":
		return len(c.Samples)
	case "subject_areas":
		return len(c.Subjects)
	case "prompts":
		return len(c.Prompts)
	case "databases":
		return len(c.Databases)
	case "glossary":
		return glossarySize(c.Glossary)
	case "metrics":
		return len(c.Metrics)
	case "patterns":
		return len(c.Patterns)
	case "learned_rules":
		return len(c.LearnedRules)
	case "feedback":
		return len(c.FeedbackUsage)
	}
	return nil
}

// DatasetStatus returns the registry merged with live state: file presence,
// size, mtime, loaded entry counts, and load issues attributed to the file.
func (c *Catalog) DatasetStatus() []map[string]any {
	out := make([]map[string]any, 0, len(DatasetRegistry))
	for _, d := range DatasetRegistry {
		path := filepath.Join(c.DataDir, d.File)
		entry := map[string]any{
			"name":        d.Name,
			"file":        d.File,
			"required":    d.Required,
			"editable":    d.Editable,
			"format":      d.Format,
			"description": d.Description,
			"schema":      d.Schema,
			"used_by":     d.UsedBy,
		}
		if st, err := os.Stat(path); err == nil {
			entry["present"] = true
			if st.IsDir() {
				n := 0
				if files, err := os.ReadDir(path); err == nil {
					for _, f := range files {
						if strings.HasSuffix(f.Name(), ".jsonl") {
							n++
						}
					}
				}
				entry["files"] = n
			} else {
				entry["size_bytes"] = st.Size()
				entry["modified"] = st.ModTime().Format(time.RFC3339)
			}
		} else {
			entry["present"] = false
			if !d.Required {
				entry["note"] = "선택 파일: 없으면 해당 기능이 축소되거나 내장 기본값 사용"
			}
		}
		if n := c.datasetLoadedCount(d.Name); n != nil {
			entry["loaded"] = n
		}
		var issues []LoadIssue
		for _, i := range c.Issues {
			if i.Source == d.File {
				issues = append(issues, i)
			}
		}
		if len(issues) > 0 {
			entry["issues"] = issues
		}
		out = append(out, entry)
	}
	return out
}

// DatasetSample returns registry info plus the head of the file's content so
// operators can inspect a dataset without shell access.
func (c *Catalog) DatasetSample(name string, sampleRows int) (map[string]any, error) {
	d, ok := findDataset(name)
	if !ok {
		return nil, fmt.Errorf("unknown dataset %q; call list_datasets for valid names", name)
	}
	if sampleRows <= 0 {
		sampleRows = 5
	}
	if sampleRows > 50 {
		sampleRows = 50
	}
	res := map[string]any{
		"name": d.Name, "file": d.File, "required": d.Required, "editable": d.Editable,
		"format": d.Format, "description": d.Description, "schema": d.Schema, "used_by": d.UsedBy,
	}
	path := filepath.Join(c.DataDir, d.File)
	if d.Format == "jsonl-dir" {
		files, err := os.ReadDir(path)
		if err != nil {
			res["present"] = false
			return res, nil
		}
		names := []string{}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".jsonl") {
				names = append(names, f.Name())
			}
		}
		res["present"] = true
		res["files"] = names
		return res, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		res["present"] = false
		return res, nil
	}
	res["present"] = true
	res["size_bytes"] = len(b)
	var parsed any
	if err := json.Unmarshal(b, &parsed); err != nil {
		res["parse_error"] = err.Error()
		return res, nil
	}
	switch v := parsed.(type) {
	case []any:
		res["total_entries"] = len(v)
		if len(v) > sampleRows {
			v = v[:sampleRows]
		}
		res["sample"] = v
	default:
		res["sample"] = v
	}
	return res, nil
}

// ReplaceDataset validates and writes new content for a registered dataset,
// backing up the previous file. The caller must then rebuild the catalog via
// Load and roll back with the returned backup path if compilation fails.
func ReplaceDataset(dataDir, name string, content json.RawMessage) (DatasetInfo, string, error) {
	d, ok := findDataset(name)
	if !ok {
		return d, "", fmt.Errorf("unknown dataset %q; call list_datasets for valid names", name)
	}
	if !d.Editable {
		return d, "", errors.New("dataset " + d.Name + " is system-managed and cannot be replaced")
	}
	if len(content) == 0 || !json.Valid(content) {
		return d, "", errors.New("content is not valid JSON")
	}
	var parsed any
	_ = json.Unmarshal(content, &parsed)
	switch d.Format {
	case "json-array":
		if _, ok := parsed.([]any); !ok {
			return d, "", errors.New("dataset " + d.Name + " expects a JSON array: " + d.Schema)
		}
	case "json-object":
		if _, ok := parsed.(map[string]any); !ok {
			return d, "", errors.New("dataset " + d.Name + " expects a JSON object: " + d.Schema)
		}
	}
	path := filepath.Join(dataDir, d.File)
	backup, err := backupDatasetFile(dataDir, d.File)
	if err != nil {
		return d, "", err
	}
	pretty, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return d, backup, err
	}
	if err := os.WriteFile(path, append(pretty, '\n'), 0o644); err != nil {
		return d, backup, err
	}
	return d, backup, nil
}

// RemoveDataset deletes an optional, editable dataset file after backing it
// up. Required datasets are refused.
func RemoveDataset(dataDir, name string) (DatasetInfo, string, error) {
	d, ok := findDataset(name)
	if !ok {
		return d, "", fmt.Errorf("unknown dataset %q; call list_datasets for valid names", name)
	}
	if d.Required {
		return d, "", errors.New("dataset " + d.Name + " is required; the server cannot start without it")
	}
	if !d.Editable {
		return d, "", errors.New("dataset " + d.Name + " is system-managed and cannot be removed")
	}
	path := filepath.Join(dataDir, d.File)
	if _, err := os.Stat(path); err != nil {
		return d, "", errors.New("dataset file is not present: " + d.File)
	}
	backup, err := backupDatasetFile(dataDir, d.File)
	if err != nil {
		return d, "", err
	}
	if err := os.Remove(path); err != nil {
		return d, backup, err
	}
	return d, backup, nil
}

// RestoreDatasetBackup copies a backup back over the live file (rollback).
func RestoreDatasetBackup(dataDir, file, backup string) error {
	if backup == "" {
		return errors.New("no backup available")
	}
	b, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, file), b, 0o644)
}

// DatasetContent returns the raw file bytes of a registered dataset for the
// admin editor. jsonl-dir datasets are refused (use the audit/feedback files
// directly).
func DatasetContent(dataDir, name string) (DatasetInfo, []byte, error) {
	d, ok := findDataset(name)
	if !ok {
		return d, nil, fmt.Errorf("unknown dataset %q", name)
	}
	if d.Format == "jsonl-dir" {
		return d, nil, errors.New("dataset " + d.Name + " is a directory of JSONL files; it has no single content document")
	}
	b, err := os.ReadFile(filepath.Join(dataDir, d.File))
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil, nil
		}
		return d, nil, err
	}
	return d, b, nil
}

// BackupEntry describes one stored backup of a dataset file.
type BackupEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size_bytes"`
	Modified string `json:"modified"`
}

// ListDatasetBackups returns backups recorded for one dataset, newest first.
func ListDatasetBackups(dataDir, name string) (DatasetInfo, []BackupEntry, error) {
	d, ok := findDataset(name)
	if !ok {
		return d, nil, fmt.Errorf("unknown dataset %q", name)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil {
		return d, []BackupEntry{}, nil
	}
	var out []BackupEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), d.File+".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupEntry{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().Format(time.RFC3339)})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []BackupEntry{}
	}
	return d, out, nil
}

// ResolveBackupPath validates that a backup name belongs to the dataset and
// contains no path separators, preventing traversal, and returns its path.
func ResolveBackupPath(dataDir, name, backupName string) (DatasetInfo, string, error) {
	d, ok := findDataset(name)
	if !ok {
		return d, "", fmt.Errorf("unknown dataset %q", name)
	}
	if backupName == "" || strings.ContainsAny(backupName, "/\\") || !strings.HasPrefix(backupName, d.File+".") {
		return d, "", errors.New("invalid backup name for dataset " + d.Name)
	}
	path := filepath.Join(dataDir, "backups", backupName)
	if _, err := os.Stat(path); err != nil {
		return d, "", errors.New("backup not found: " + backupName)
	}
	return d, path, nil
}

func backupDatasetFile(dataDir, file string) (string, error) {
	src := filepath.Join(dataDir, file)
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // nothing to back up
		}
		return "", err
	}
	dir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, file+"."+time.Now().Format("20060102T150405"))
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}
