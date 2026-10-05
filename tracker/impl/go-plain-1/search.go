package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

var sortColumns = map[string]string{
	"created":  "i.created_at",
	"updated":  "i.updated_at",
	"priority": "i.priority_rank",
}

func (s *Server) searchIssues(w http.ResponseWriter, r *http.Request, uid string) error {
	ctx := r.Context()
	org, err := s.loadOrg(ctx, r.PathValue("slug"), uid)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	v := &verrs{}
	pg := parsePage(q, v)

	var conds []string
	var args []any
	arg := func(x any) string {
		args = append(args, x)
		return "$" + strconv.Itoa(len(args))
	}
	list := func(name string) []string {
		var out []string
		for _, part := range strings.Split(q.Get(name), ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	enumFilter := func(name, col string, allowed []string) {
		vals := list(name)
		if len(vals) == 0 {
			return
		}
		for _, x := range vals {
			if !contains(allowed, x) {
				v.add(name, "invalid")
				return
			}
		}
		conds = append(conds, col+" = ANY("+arg(vals)+"::text[])")
	}
	userFilter := func(name string, allowNone bool) {
		vals := list(name)
		if len(vals) == 0 {
			return
		}
		var ids []string
		none := false
		for _, x := range vals {
			switch {
			case x == "me":
				ids = append(ids, uid)
			case x == "none" && allowNone:
				none = true
			case isUUID(x):
				ids = append(ids, strings.ToLower(x))
			default:
				v.add(name, "invalid")
				return
			}
		}
		col := "i." + name + "_id"
		var parts []string
		if len(ids) > 0 {
			parts = append(parts, col+" = ANY("+arg(ids)+"::uuid[])")
		}
		if none {
			parts = append(parts, col+" IS NULL")
		}
		conds = append(conds, "("+strings.Join(parts, " OR ")+")")
	}
	timeFilter := func(name, cond string) {
		raw := q.Get(name)
		if raw == "" {
			return
		}
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			v.add(name, "invalid")
			return
		}
		conds = append(conds, cond+arg(t))
	}

	visible, err := s.visibleProjects(ctx, org, uid)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, k := range list("project") {
		want[strings.ToUpper(k)] = true
	}
	ids := []string{}
	for _, p := range visible {
		if len(want) == 0 || want[p.Key] {
			ids = append(ids, p.ID)
		}
	}
	conds = append(conds, "i.project_id = ANY("+arg(ids)+"::uuid[])")
	enumFilter("status", "i.status", issueStatus)
	enumFilter("priority", "i.priority", priorities)
	enumFilter("type", "i.type", issueTypes)
	userFilter("assignee", true)
	userFilter("reporter", false)
	if labels := list("label"); len(labels) > 0 {
		for n := range labels {
			labels[n] = strings.ToLower(labels[n])
		}
		conds = append(conds, "i.labels && "+arg(labels)+"::text[]")
	}
	timeFilter("created_after", "i.created_at > ")
	timeFilter("created_before", "i.created_at < ")
	timeFilter("updated_after", "i.updated_at > ")
	if ws := words(q.Get("q")); len(ws) > 0 {
		conds = append(conds, "i.search_words @> "+arg(ws)+"::text[]")
	}

	order := "i.created_at DESC"
	if sv := q.Get("sort"); sv != "" {
		desc := strings.HasPrefix(sv, "-")
		field := strings.TrimPrefix(sv, "-")
		dir := " ASC"
		if desc {
			dir = " DESC"
		}
		if field == "key" {
			order = "p.key" + dir + ", i.number" + dir
		} else if col, ok := sortColumns[field]; ok {
			order = col + dir
		} else {
			v.add("sort", "invalid")
		}
	}
	if !v.ok() {
		return v.err()
	}
	order += ", p.key ASC, i.number ASC"
	where := " WHERE " + strings.Join(conds, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM issues i JOIN projects p ON p.id = i.project_id`+where,
		args...).Scan(&total); err != nil {
		return err
	}
	issues, err := queryIssues(ctx, s.pool, issueSelect+where+" ORDER BY "+order+pg.sql(), args...)
	if err != nil {
		return err
	}
	items := make([]map[string]any, len(issues))
	for n, i := range issues {
		items[n] = i.JSON()
	}
	resp := finish(pg, items)
	resp["total"] = total
	writeJSON(w, 200, resp)
	return nil
}
