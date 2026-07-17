package catalog

import (
	"errors"
	"strings"
)

type JoinPathRequest struct {
	FromTable string   `json:"from_table,omitempty"`
	ToTables  []string `json:"to_tables,omitempty"`
	Tables    []string `json:"tables,omitempty"`
	MaxDepth  int      `json:"max_depth,omitempty"`
}

type JoinPathResult struct {
	From       string         `json:"from"`
	To         string         `json:"to"`
	Found      bool           `json:"found"`
	Depth      int            `json:"depth,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	Edges      []JoinPathEdge `json:"edges,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Guidance   string         `json:"guidance,omitempty"`
}

type JoinPathEdge struct {
	From          string  `json:"from"`
	To            string  `json:"to"`
	FromColumn    string  `json:"from_column,omitempty"`
	ToColumn      string  `json:"to_column,omitempty"`
	JoinType      string  `json:"join_type,omitempty"`
	Cardinality   string  `json:"cardinality,omitempty"`
	Condition     string  `json:"condition"`
	Preferred     bool    `json:"preferred,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
	Description   string  `json:"description,omitempty"`
	Caution       string  `json:"caution,omitempty"`
	ProvisionType string  `json:"provision_type,omitempty"`
}

// GetJoinPaths returns catalog-backed join paths between every pair of the
// requested tables. SQL generation must take ON conditions from here; when a
// pair has no path or low confidence, the guidance says what to do instead.
func (c *Catalog) GetJoinPaths(req JoinPathRequest) (map[string]any, error) {
	if req.MaxDepth <= 0 {
		req.MaxDepth = 3
	}
	pairs := [][2]string{}
	if len(req.Tables) > 1 {
		for i := 0; i < len(req.Tables); i++ {
			for j := i + 1; j < len(req.Tables); j++ {
				pairs = append(pairs, [2]string{req.Tables[i], req.Tables[j]})
			}
		}
	} else if req.FromTable != "" && len(req.ToTables) > 0 {
		for _, to := range req.ToTables {
			pairs = append(pairs, [2]string{req.FromTable, to})
		}
	} else {
		return nil, errors.New("provide either tables with at least two entries, or from_table plus to_tables")
	}
	results := []JoinPathResult{}
	allFound := true
	lowConfidence := false
	for _, pair := range pairs {
		from, ok := c.ResolveTable(pair[0])
		if !ok {
			results = append(results, JoinPathResult{From: pair[0], To: pair[1], Reason: "from_table not found"})
			allFound = false
			continue
		}
		to, ok := c.ResolveTable(pair[1])
		if !ok {
			results = append(results, JoinPathResult{From: from.FQN, To: pair[1], Reason: "to_table not found"})
			allFound = false
			continue
		}
		if fj, bad := c.IsForbiddenJoin(from.FQN, to.FQN); bad {
			results = append(results, JoinPathResult{
				From: from.FQN, To: to.FQN, Found: false,
				Reason:   "join is forbidden by operator policy: " + fj.Reason,
				Guidance: "이 조합은 조인할 수 없습니다. 단일 테이블 쿼리로 재구성하거나 다른 테이블 조합을 사용하세요.",
			})
			allFound = false
			continue
		}
		r := c.findJoinPath(from.FQN, to.FQN, req.MaxDepth)
		if !r.Found {
			allFound = false
			r.Guidance = "카탈로그에 조인 경로가 없습니다. 임의의 조인 조건을 만들지 말고, 사용자에게 연결 기준을 확인하거나 각 테이블을 단일 테이블 쿼리로 분리하세요."
		} else if r.Confidence < 0.7 {
			lowConfidence = true
			r.Guidance = "조인 confidence가 낮습니다. SQL을 바로 생성하지 말고 조인 기준이 맞는지 사용자에게 확인하거나, 주 테이블 단일 쿼리 대안을 제시하세요."
		}
		results = append(results, r)
	}
	res := map[string]any{"join_paths": results}
	switch {
	case !allFound:
		res["overall_guidance"] = "일부 테이블 쌍에 검증된 조인 경로가 없습니다. 경로가 없는 테이블은 SQL에서 제외하거나 사용자에게 조인 기준을 되물어야 합니다."
	case lowConfidence:
		res["overall_guidance"] = "낮은 confidence의 조인이 포함되어 있습니다. 적용 전 조인 조건의 업무적 타당성을 확인하세요."
	default:
		res["overall_guidance"] = "모든 조인 경로가 카탈로그에서 검증되었습니다. 제공된 condition을 그대로 사용하세요."
	}
	return res, nil
}

func (c *Catalog) findJoinPath(from, to string, maxDepth int) JoinPathResult {
	if from == to {
		return JoinPathResult{From: from, To: to, Found: true, Depth: 0, Confidence: 1}
	}
	type node struct {
		Table string
		Path  []JoinEdge
	}
	q := []node{{Table: from}}
	seen := map[string]bool{from: true}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if len(cur.Path) >= maxDepth {
			continue
		}
		for _, edge := range c.Adjacency[cur.Table] {
			if seen[edge.To] {
				continue
			}
			nextPath := append(append([]JoinEdge{}, cur.Path...), edge)
			if edge.To == to {
				return JoinPathResult{
					From: from, To: to, Found: true, Depth: len(nextPath),
					Confidence: pathConfidence(nextPath),
					Edges:      renderJoinPath(nextPath),
				}
			}
			seen[edge.To] = true
			q = append(q, node{Table: edge.To, Path: nextPath})
		}
	}
	return JoinPathResult{From: from, To: to, Found: false, Reason: "no relation path found within max_depth"}
}

// pathConfidence is the minimum edge confidence, discounted for longer paths.
func pathConfidence(path []JoinEdge) float64 {
	conf := 1.0
	for _, e := range path {
		ec := e.Relation.Confidence
		if ec == 0 {
			ec = 0.6
		}
		if ec < conf {
			conf = ec
		}
	}
	if len(path) > 1 {
		conf -= 0.05 * float64(len(path)-1)
	}
	if conf < 0 {
		conf = 0
	}
	return round(conf)
}

func renderJoinPath(path []JoinEdge) []JoinPathEdge {
	out := make([]JoinPathEdge, 0, len(path))
	for _, edge := range path {
		fromCol, toCol := edge.Relation.BaseColumn, edge.Relation.ReferenceColumn
		if edge.Reversed {
			fromCol, toCol = toCol, fromCol
		}
		out = append(out, JoinPathEdge{
			From:          edge.From,
			To:            edge.To,
			FromColumn:    fromCol,
			ToColumn:      toCol,
			JoinType:      nonEmpty(edge.Relation.JoinType, "INNER"),
			Cardinality:   edge.Relation.Cardinality,
			Condition:     edgeCondition(edge, "", ""),
			Preferred:     edge.Relation.Preferred,
			Confidence:    edge.Relation.Confidence,
			Description:   edge.Relation.Description,
			Caution:       edge.Relation.Caution,
			ProvisionType: edge.Relation.ProvisionType,
		})
	}
	return out
}

func edgeCondition(edge JoinEdge, fromAlias, toAlias string) string {
	if fromAlias == "" {
		fromAlias = edge.From
	}
	if toAlias == "" {
		toAlias = edge.To
	}
	var leftCols, rightCols []string
	if edge.Reversed {
		leftCols = splitColumns(edge.Relation.ReferenceColumn)
		rightCols = splitColumns(edge.Relation.BaseColumn)
	} else {
		leftCols = splitColumns(edge.Relation.BaseColumn)
		rightCols = splitColumns(edge.Relation.ReferenceColumn)
	}
	n := len(leftCols)
	if len(rightCols) < n {
		n = len(rightCols)
	}
	if n == 0 {
		return fromAlias + " = " + toAlias
	}
	conds := make([]string, 0, n)
	for i := 0; i < n; i++ {
		conds = append(conds, fromAlias+"."+leftCols[i]+" = "+toAlias+"."+rightCols[i])
	}
	return strings.Join(conds, " AND ")
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
