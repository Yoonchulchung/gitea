// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"strconv"
	"testing"

	"gitea.dev/company"
	repo_model "gitea.dev/models/repo"
	"gitea.dev/models/unittest"
	user_model "gitea.dev/models/user"
	"gitea.dev/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompanyProjects(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	ctx := t.Context()

	owner := loginUser(t, "user4")
	owner.MakeRequest(t, NewRequestWithValues(t, "POST", "/company/projects/new", map[string]string{
		"repo_name": "inventory",
	}), http.StatusSeeOther)
	org, err := company.ProjectOrg(ctx)
	require.NoError(t, err)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{OwnerID: org.ID, Name: "inventory"})
	assertOwner := func(name string) {
		t.Helper()
		u, err := company.ProjectOwnerOf(ctx, repo.ID)
		require.NoError(t, err)
		require.NotNil(t, u)
		assert.Equal(t, name, u.Name)
	}
	assertOwner("user4")
	owner.MakeRequest(t, NewRequest(t, "GET", repo.Link()), http.StatusOK)

	member := loginUser(t, "user5")
	member.MakeRequest(t, NewRequest(t, "GET", repo.Link()), http.StatusNotFound)
	membersURL := "/company/project/" + repo.FullName() + "/members"
	member.MakeRequest(t, NewRequestWithValues(t, "POST", membersURL, map[string]string{"user": "user5"}), http.StatusNotFound)
	owner.MakeRequest(t, NewRequestWithValues(t, "POST", membersURL, map[string]string{"user": "user5"}), http.StatusSeeOther)
	member.MakeRequest(t, NewRequest(t, "GET", repo.Link()), http.StatusOK)

	t.Run("AdminNamesAnotherOwner", func(t *testing.T) {
		admin := loginUser(t, "user1")
		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/company-projects/owner", map[string]string{
			"repo_id": strconv.FormatInt(repo.ID, 10), "owner": "user5", "drop_previous": "on",
		}), http.StatusSeeOther)
		assertOwner("user5")
		owner.MakeRequest(t, NewRequest(t, "GET", repo.Link()), http.StatusNotFound)

		admin.MakeRequest(t, NewRequestWithValues(t, "POST", "/-/admin/company-projects/handover", map[string]string{
			"from_id": "5", "to": "user2",
		}), http.StatusSeeOther)
		assertOwner("user2")
		member.MakeRequest(t, NewRequest(t, "GET", repo.Link()), http.StatusOK) // still a member
	})

	t.Run("DepartmentRepoMoves", func(t *testing.T) {
		dept := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{OwnerName: "org3", Name: "repo3"})
		admin := unittest.AssertExistsAndLoadBean(t, &user_model.User{Name: "user1"})
		require.NoError(t, company.MigrateRepoToProject(ctx, admin, dept))
		moved := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: dept.ID})
		assert.Equal(t, org.ID, moved.OwnerID)
		for _, uid := range []int64{2, 4, 28} { // org3's members
			ok, err := repo_model.IsCollaborator(ctx, moved.ID, uid)
			require.NoError(t, err)
			assert.True(t, ok, "user %d keeps access", uid)
		}
		resp := loginUser(t, "user4").MakeRequest(t, NewRequest(t, "GET", "/org3/repo3"), http.StatusMovedPermanently)
		assert.Equal(t, "/projects/repo3", resp.Header().Get("Location"))
	})

	t.Run("NoDepartmentPages", func(t *testing.T) {
		resp := member.MakeRequest(t, NewRequest(t, "GET", "/org/org3/dashboard"), http.StatusSeeOther)
		assert.Equal(t, "/", resp.Header().Get("Location"))
	})

	t.Run("NoSignUp", func(t *testing.T) {
		MakeRequest(t, NewRequest(t, "GET", "/user/sign_up"), http.StatusNotFound)
		resp := MakeRequest(t, NewRequest(t, "GET", "/user/login"), http.StatusOK)
		assert.NotContains(t, resp.Body.String(), "/user/sign_up")
	})
}
