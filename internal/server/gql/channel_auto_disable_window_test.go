package gql

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/stretchr/testify/require"
)

// Exercise generated input decoding, normalization, JSON persistence and output
// fields together: edit forms must be able to read back the rules they submit.
func TestAutoDisableWindowGraphQLRoundTrip(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), db))
	service := biz.NewChannelServiceForTest(db)
	t.Cleanup(service.Stop)
	handler := NewGraphqlHandlers(Dependencies{Ent: db, ChannelService: service})
	post := func(query string, variables map[string]any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/admin/graphql", bytes.NewReader(body)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.Graphql.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Empty(t, result["errors"], response.Body.String())
		return result["data"].(map[string]any)
	}
	selection := `id policies { apiKeyAutoDisableMode apiKeyAutoDisableRules { times action windowMinutes freezeModel disableDurationMinutes } }`
	created := post(`mutation($input: CreateChannelInput!) { createChannel(input: $input) { `+selection+` } }`, map[string]any{
		"input": map[string]any{"name": "window-model-roundtrip", "type": "openai",
			"credentials": map[string]any{"apiKey": "test-key"}, "supportedModels": []string{"test-model"}, "defaultTestModel": "test-model",
			"policies": map[string]any{"apiKeyAutoDisableMode": "custom", "apiKeyAutoDisableRules": []map[string]any{{
				"times": 3, "action": "temporary_disable", "disableDurationMinutes": 15, "windowMinutes": 10, "freezeModel": true,
			}}},
		},
	})["createChannel"].(map[string]any)
	assertRule := func(value map[string]any, window float64, freeze bool) {
		t.Helper()
		policies := value["policies"].(map[string]any)
		require.Equal(t, "custom", policies["apiKeyAutoDisableMode"])
		rules := policies["apiKeyAutoDisableRules"].([]any)
		require.Len(t, rules, 1)
		rule := rules[0].(map[string]any)
		require.Equal(t, window, rule["windowMinutes"])
		require.Equal(t, freeze, rule["freezeModel"])
		require.Equal(t, float64(3), rule["times"])
	}
	assertRule(created, 10, true)
	read := post(`query { channels(first: 10) { edges { node { `+selection+` } } } }`, nil)
	edges := read["channels"].(map[string]any)["edges"].([]any)
	require.Len(t, edges, 1)
	assertRule(edges[0].(map[string]any)["node"].(map[string]any), 10, true)
	updated := post(`mutation($id: ID!, $input: UpdateChannelInput!) { updateChannel(id: $id, input: $input) { `+selection+` } }`, map[string]any{
		"id": created["id"], "input": map[string]any{"policies": map[string]any{
			"apiKeyAutoDisableMode": "custom", "apiKeyAutoDisableRules": []map[string]any{{
				"times": 3, "action": "temporary_disable", "disableDurationMinutes": 15, "windowMinutes": 0, "freezeModel": false,
			}},
		}},
	})["updateChannel"].(map[string]any)
	assertRule(updated, 10, true)
}
