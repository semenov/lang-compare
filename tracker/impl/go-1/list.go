package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type sortSpec struct {
	col  string // "" for key
	desc bool
}

func hListIssues(w http.ResponseWriter, r *http.Request, uid UUID) {
	ctx := r.Context()
	q := r.URL.Query()
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}
	slug := r.PathValue("slug")

	// caller's org role and visible projects in one round trip
	var oid UUID
	var orgRole int16
	var pids []UUID
	var pkeys []string
	err := db.QueryRow(ctx, `SELECT o.id, om.role,
		ARRAY(SELECT p.id FROM projects p WHERE p.org_id=o.id AND (om.role >= 2 OR p.visibility=0 OR EXISTS
			(SELECT 1 FROM project_members pm WHERE pm.project_id=p.id AND pm.user_id=$2)) ORDER BY p.key),
		ARRAY(SELECT p.key FROM projects p WHERE p.org_id=o.id AND (om.role >= 2 OR p.visibility=0 OR EXISTS
			(SELECT 1 FROM project_members pm WHERE pm.project_id=p.id AND pm.user_id=$2)) ORDER BY p.key)
		FROM orgs o JOIN org_members om ON om.org_id=o.id AND om.user_id=$2 WHERE o.slug=$1`, slug, uid).
		Scan(&oid, &orgRole, &pids, &pkeys)
	if err == pgx.ErrNoRows {
		errNotFound(w)
		return
	}
	if err != nil {
		errInternal(w, err)
		return
	}

	var v verrs
	args := []any{oid}
	where := []string{"i.org_id=$1"}
	arg := func(a any) string {
		args = append(args, a)
		return "$" + strconv.Itoa(len(args))
	}

	allProjects := orgRole >= orgAdmin
	vis := pids
	if s := q.Get("project"); s != "" {
		want := map[string]bool{}
		for _, k := range strings.Split(s, ",") {
			want[strings.ToUpper(strings.TrimSpace(k))] = true
		}
		vis = nil
		for i, k := range pkeys {
			if want[k] {
				vis = append(vis, pids[i])
			}
		}
		allProjects = false
	}
	if !allProjects {
		if len(vis) == 0 {
			writeJSON(w, 200, []byte(`{"total":0,"items":[],"next_cursor":null}`))
			return
		}
		if len(vis) == 1 {
			where = append(where, "i.project_id="+arg(vis[0]))
		} else {
			where = append(where, "i.project_id = ANY("+arg(vis)+")")
		}
	}
	enumFilter := func(name, col string, names []string) {
		s := q.Get(name)
		if s == "" {
			return
		}
		var vals []int16
		for _, p := range strings.Split(s, ",") {
			i := enumIndex(names, strings.TrimSpace(p))
			if i < 0 {
				v.add(name, "invalid")
				return
			}
			vals = append(vals, i)
		}
		if len(vals) == 1 {
			where = append(where, col+"="+arg(vals[0]))
		} else {
			where = append(where, col+" = ANY("+arg(vals)+")")
		}
	}
	enumFilter("status", "i.status", statusNames)
	enumFilter("priority", "i.priority", priorityNames)
	enumFilter("type", "i.type", typeNames)
	userFilter := func(name, col string, allowNone bool) {
		s := q.Get(name)
		if s == "" {
			return
		}
		var ids []UUID
		none := false
		for _, p := range strings.Split(s, ",") {
			p = strings.TrimSpace(p)
			switch {
			case p == "me":
				ids = append(ids, uid)
			case p == "none" && allowNone:
				none = true
			default:
				u, ok := parseUUID(p)
				if !ok {
					v.add(name, "invalid")
					return
				}
				ids = append(ids, u)
			}
		}
		var conds []string
		if len(ids) == 1 {
			conds = append(conds, col+"="+arg(ids[0]))
		} else if len(ids) > 1 {
			conds = append(conds, col+" = ANY("+arg(ids)+")")
		}
		if none {
			conds = append(conds, col+" IS NULL")
		}
		where = append(where, "("+strings.Join(conds, " OR ")+")")
	}
	userFilter("assignee", "i.assignee_id", true)
	userFilter("reporter", "i.reporter_id", false)
	if s := q.Get("label"); s != "" {
		var ls []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				ls = append(ls, p)
			}
		}
		where = append(where, "i.labels && "+arg(ls)+"::text[]")
	}
	timeFilter := func(name, cond string) {
		s := q.Get(name)
		if s == "" {
			return
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			v.add(name, "invalid")
			return
		}
		where = append(where, cond+arg(t))
	}
	timeFilter("created_after", "i.created_at > ")
	timeFilter("created_before", "i.created_at < ")
	timeFilter("updated_after", "i.updated_at > ")
	if s := q.Get("q"); s != "" {
		if ws := queryWords(s); len(ws) > 0 {
			where = append(where, "i.words @> "+arg(ws)+"::text[]")
		}
	}
	var srt sortSpec
	sortS := q.Get("sort")
	if sortS == "" {
		sortS = "-created"
	}
	{
		f := strings.TrimPrefix(sortS, "-")
		srt.desc = len(f) != len(sortS)
		switch f {
		case "created":
			srt.col = "i.created_at"
		case "updated":
			srt.col = "i.updated_at"
		case "priority":
			srt.col = "i.priority"
		case "key":
		default:
			v.add("sort", "invalid")
		}
	}
	if len(v) > 0 {
		errValidation(w, v)
		return
	}
	countWhere := strings.Join(where, " AND ")
	countArgs := append([]any(nil), args...)

	// keyset cursor: "<sortval>|<project key>|<number>"
	if cs := q.Get("cursor"); cs != "" {
		raw, ok1 := decCursor(cs)
		parts := strings.Split(raw, "|")
		var num int64
		var err error
		if ok1 && len(parts) == 3 {
			num, err = strconv.ParseInt(parts[2], 10, 32)
		}
		if !ok1 || len(parts) != 3 || err != nil {
			errValidation1(w, "cursor", "invalid")
			return
		}
		pk, n := arg(parts[1]), arg(int32(num))
		if srt.col == "" {
			if srt.desc {
				where = append(where, "(i.project_key, i.number) < ("+pk+", "+n+")")
			} else {
				where = append(where, "(i.project_key, i.number) > ("+pk+", "+n+")")
			}
		} else {
			var sv string
			if srt.col == "i.priority" {
				p, err := strconv.Atoi(parts[0])
				if err != nil {
					errValidation1(w, "cursor", "invalid")
					return
				}
				sv = arg(int16(p))
			} else {
				us, err := strconv.ParseInt(parts[0], 10, 64)
				if err != nil {
					errValidation1(w, "cursor", "invalid")
					return
				}
				sv = arg(time.UnixMicro(us).UTC())
			}
			op := ">"
			if srt.desc {
				op = "<"
			}
			where = append(where, srt.col+" "+op+"= "+sv+" AND ("+srt.col+" "+op+" "+sv+
				" OR (i.project_key, i.number) > ("+pk+", "+n+"))")
		}
	}
	order := ""
	if srt.col == "" {
		if srt.desc {
			order = "i.project_key DESC, i.number DESC"
		} else {
			order = "i.project_key, i.number"
		}
	} else {
		dir := ""
		if srt.desc {
			dir = " DESC"
		}
		order = srt.col + dir + ", i.project_key, i.number"
	}
	sql := "SELECT " + issueCols + " FROM issues i WHERE " + strings.Join(where, " AND ") + " ORDER BY " + order +
		" LIMIT " + strconv.Itoa(limit+1)

	batch := &pgx.Batch{}
	batch.Queue("SELECT count(*) FROM issues i WHERE "+countWhere, countArgs...)
	batch.Queue(sql, args...)
	br := db.SendBatch(ctx, batch)
	defer br.Close()
	var total int64
	if err := br.QueryRow().Scan(&total); err != nil {
		errInternal(w, err)
		return
	}
	rows, err := br.Query()
	if err != nil {
		errInternal(w, err)
		return
	}
	out := make([]byte, 0, 1024*limit/2+64)
	out = append(out, `{"total":`...)
	out = appendInt(out, total)
	out = append(out, `,"items":[`...)
	var x, last Issue
	n := 0
	more := false
	for rows.Next() {
		if err := rows.Scan(x.dests()...); err != nil {
			rows.Close()
			errInternal(w, err)
			return
		}
		if n == limit {
			more = true
			break
		}
		if n > 0 {
			out = append(out, ',')
		}
		out = appendIssue(out, &x)
		last = x
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		errInternal(w, err)
		return
	}
	out = append(out, `],"next_cursor":`...)
	if more {
		var c string
		switch srt.col {
		case "i.priority":
			c = strconv.Itoa(int(last.Priority))
		case "i.created_at":
			c = strconv.FormatInt(last.Created.UnixMicro(), 10)
		case "i.updated_at":
			c = strconv.FormatInt(last.Updated.UnixMicro(), 10)
		}
		c += "|" + last.ProjectKey + "|" + strconv.Itoa(int(last.Number))
		out = appendStr(out, encCursor(c))
	} else {
		out = append(out, "null"...)
	}
	out = append(out, '}')
	writeJSON(w, 200, out)
}
