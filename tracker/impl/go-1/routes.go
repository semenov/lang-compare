package main

import (
	"context"
	"net/http"
	"time"
)

func routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []byte(`{"status":"ok"}`))
	})
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			writeJSON(w, 503, []byte(`{"status":"unavailable"}`))
			return
		}
		writeJSON(w, 200, []byte(`{"status":"ok"}`))
	})
	const p = "/api/v1"
	m.HandleFunc("POST "+p+"/auth/register", hRegister)
	m.HandleFunc("POST "+p+"/auth/login", hLogin)
	m.HandleFunc("POST "+p+"/auth/refresh", hRefresh)
	m.HandleFunc("POST "+p+"/auth/logout", hLogout)
	a := func(pattern string, h authedHandler) { m.HandleFunc(pattern, authed(h)) }
	a("GET "+p+"/me", hMe)
	a("PATCH "+p+"/me", hPatchMe)
	a("POST "+p+"/orgs", hCreateOrg)
	a("GET "+p+"/orgs", hListOrgs)
	a("GET "+p+"/orgs/{slug}", hGetOrg)
	a("GET "+p+"/orgs/{slug}/members", hListOrgMembers)
	a("POST "+p+"/orgs/{slug}/members", hAddOrgMember)
	a("PATCH "+p+"/orgs/{slug}/members/{user_id}", hPatchOrgMember)
	a("DELETE "+p+"/orgs/{slug}/members/{user_id}", hDeleteOrgMember)
	a("POST "+p+"/orgs/{slug}/projects", hCreateProject)
	a("GET "+p+"/orgs/{slug}/projects", hListProjects)
	a("GET "+p+"/orgs/{slug}/projects/{key}", hGetProject)
	a("PATCH "+p+"/orgs/{slug}/projects/{key}", hPatchProject)
	a("DELETE "+p+"/orgs/{slug}/projects/{key}", hDeleteProject)
	a("GET "+p+"/orgs/{slug}/projects/{key}/members", hListProjectMembers)
	a("PUT "+p+"/orgs/{slug}/projects/{key}/members/{user_id}", hPutProjectMember)
	a("DELETE "+p+"/orgs/{slug}/projects/{key}/members/{user_id}", hDeleteProjectMember)
	a("POST "+p+"/orgs/{slug}/projects/{key}/issues", hCreateIssue)
	a("POST "+p+"/orgs/{slug}/projects/{key}/issues/bulk", hBulkCreate)
	a("GET "+p+"/orgs/{slug}/issues", hListIssues)
	a("GET "+p+"/orgs/{slug}/issues/{ikey}", hGetIssue)
	a("PATCH "+p+"/orgs/{slug}/issues/{ikey}", hPatchIssue)
	a("DELETE "+p+"/orgs/{slug}/issues/{ikey}", hDeleteIssue)
	a("POST "+p+"/orgs/{slug}/issues/{ikey}/transition", hTransition)
	a("POST "+p+"/orgs/{slug}/issues/{ikey}/comments", hCreateComment)
	a("GET "+p+"/orgs/{slug}/issues/{ikey}/comments", hListComments)
	a("GET "+p+"/orgs/{slug}/issues/{ikey}/history", hHistory)
	a("PATCH "+p+"/orgs/{slug}/comments/{id}", hPatchComment)
	a("DELETE "+p+"/orgs/{slug}/comments/{id}", hDeleteComment)
	a("POST "+p+"/orgs/{slug}/webhooks", hCreateHook)
	a("GET "+p+"/orgs/{slug}/webhooks", hListHooks)
	a("GET "+p+"/orgs/{slug}/webhooks/{id}", hGetHook)
	a("PATCH "+p+"/orgs/{slug}/webhooks/{id}", hPatchHook)
	a("DELETE "+p+"/orgs/{slug}/webhooks/{id}", hDeleteHook)
	a("GET "+p+"/orgs/{slug}/webhooks/{id}/deliveries", hListDeliveries)
	return m
}
