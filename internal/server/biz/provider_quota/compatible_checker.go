package provider_quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	compatibleSourceNewAPI        = "new_api"
	compatibleSourceSub2API       = "sub2api"
	compatibleSourceOpenAIBilling = "openai_billing"
	compatibleSourceDeepSeek      = "deepseek"

	// New API and One API store quota in units of 1/500000 USD.
	compatibleQuotaPerUSD = 500_000
	// Compatibility gateways commonly return a one-million-dollar sentinel as
	// their hard limit. It is not a spendable balance.
	openAIBillingSentinelLimitUSD = 1_000_000
)

// CompatibleQuotaChecker reads wallet or token balance data from common relay
// APIs. It is only used when no provider-specific checker matches the URL.
type CompatibleQuotaChecker struct {
	httpClient *httpclient.HttpClient
}

func NewCompatibleQuotaChecker(httpClient *httpclient.HttpClient) *CompatibleQuotaChecker {
	return &CompatibleQuotaChecker{httpClient: httpClient}
}

func (c *CompatibleQuotaChecker) SupportsChannel(ch *ent.Channel) bool {
	if ch == nil || !supportsCompatibleQuotaChannelType(ch.Type) {
		return false
	}

	return strings.TrimSpace(ch.BaseURL) != "" && strings.TrimSpace(ch.Credentials.APIKey) != ""
}

func (c *CompatibleQuotaChecker) CheckQuota(ctx context.Context, ch *ent.Channel) (QuotaData, error) {
	token := strings.TrimSpace(ch.Credentials.APIKey)
	if token == "" {
		return QuotaData{}, errors.New("compatible quota token is required")
	}

	origin, err := compatibleQuotaOrigin(ch.BaseURL)
	if err != nil {
		return QuotaData{}, err
	}

	hc := c.httpClient
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		hc = c.httpClient.WithProxy(ch.Settings.Proxy)
	}

	probes := []struct {
		source string
		query  func(context.Context, *httpclient.HttpClient, string, string) (QuotaData, bool, error)
	}{
		{source: compatibleSourceNewAPI, query: queryNewAPIQuota},
		{source: compatibleSourceSub2API, query: querySub2APIQuota},
		{source: compatibleSourceOpenAIBilling, query: queryOpenAIBillingQuota},
		{source: compatibleSourceDeepSeek, query: queryDeepSeekQuota},
	}

	var failures []string
	for _, probe := range probes {
		data, recognized, probeErr := probe.query(ctx, hc, origin, token)
		if recognized {
			return NormalizeQuotaData(data), nil
		}
		if probeErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", probe.source, probeErr))
		}
	}

	if len(failures) == 0 {
		return QuotaData{}, errors.New("no compatible balance endpoint returned usable quota data")
	}

	return QuotaData{}, fmt.Errorf("no compatible balance endpoint returned usable quota data (%s)", strings.Join(failures, "; "))
}

func supportsCompatibleQuotaChannelType(channelType channel.Type) bool {
	switch channelType {
	case channel.TypeOpenai, channel.TypeOpenaiResponses,
		channel.TypeDeepseek, channel.TypeDeepseekAnthropic:
		return true
	default:
		return false
	}
}

func compatibleQuotaOrigin(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid channel base URL %q", strings.TrimSpace(baseURL))
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported channel base URL scheme %q", parsed.Scheme)
	}

	parsed.Path = ""
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil

	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func queryNewAPIQuota(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
	token string,
) (QuotaData, bool, error) {
	root, err := compatibleGetJSON(ctx, hc, origin+"/api/usage/token/", token)
	if err != nil {
		return QuotaData{}, false, err
	}

	data := compatibleDataMap(root)
	totalGranted, hasGranted := compatibleNumber(data["total_granted"])
	totalUsed, hasUsed := compatibleNumber(data["total_used"])
	totalAvailable, hasAvailable := compatibleNumber(data["total_available"])
	unlimited := compatibleBool(data["unlimited_quota"]) || (hasGranted && totalGranted < 0)
	if !unlimited && !hasGranted && !hasUsed && !hasAvailable {
		return QuotaData{}, false, nil
	}

	rawData := map[string]any{
		"source":          compatibleSourceNewAPI,
		"unlimited_quota": unlimited,
	}
	if expiresAt := compatibleTime(data["expires_at"]); expiresAt != nil {
		rawData["expires_at"] = expiresAt.Format(time.RFC3339)
	}

	if unlimited {
		return QuotaData{
			Status:       "available",
			ProviderType: "compatible",
			RawData:      rawData,
			Ready:        true,
		}, true, nil
	}

	grantedUSD := totalGranted / compatibleQuotaPerUSD
	usedUSD := totalUsed / compatibleQuotaPerUSD
	availableUSD := totalAvailable / compatibleQuotaPerUSD
	if hasGranted {
		rawData["total_granted_usd"] = nonNegativeFloat(grantedUSD)
	}
	if hasUsed {
		rawData["total_used_usd"] = nonNegativeFloat(usedUSD)
	}
	if hasAvailable {
		rawData["total_available_usd"] = nonNegativeFloat(availableUSD)
	}
	if !hasAvailable && hasGranted && hasUsed {
		availableUSD = math.Max(0, grantedUSD-usedUSD)
		hasAvailable = true
		rawData["total_available_usd"] = availableUSD
	}

	if !hasAvailable {
		return QuotaData{}, false, errors.New("response did not contain an available balance")
	}

	return compatibleWalletQuota("USD", availableUSD, nullableCompatibleNumber(grantedUSD, hasGranted), nullableCompatibleNumber(usedUSD, hasUsed), rawData), true, nil
}

func querySub2APIQuota(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
	token string,
) (QuotaData, bool, error) {
	root, err := compatibleGetJSON(ctx, hc, origin+"/v1/usage", token)
	if err != nil {
		return QuotaData{}, false, err
	}

	data := compatibleDataMap(root)
	balance, hasBalance := firstCompatibleNumber(data, "balance", "remaining", "total_available")
	if !hasBalance {
		return QuotaData{}, false, nil
	}

	rawData := map[string]any{
		"source":      compatibleSourceSub2API,
		"balance_usd": nonNegativeFloat(balance),
	}
	if usage := compatibleMap(data["usage"]); usage != nil {
		if today := compatibleMap(usage["today"]); today != nil {
			if cost, ok := firstCompatibleNumber(today, "cost", "total_cost"); ok {
				rawData["today_cost_usd"] = nonNegativeFloat(cost)
			}
			if requests, ok := firstCompatibleNumber(today, "requests", "request_count"); ok {
				rawData["today_requests"] = math.Max(0, requests)
			}
		}
		if total := compatibleMap(usage["total"]); total != nil {
			if cost, ok := firstCompatibleNumber(total, "cost", "total_cost"); ok {
				rawData["total_used_usd"] = nonNegativeFloat(cost)
			}
		}
	}

	return compatibleWalletQuota("USD", balance, nil, nil, rawData), true, nil
}

func queryOpenAIBillingQuota(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
	token string,
) (QuotaData, bool, error) {
	subscription, err := compatibleGetJSON(ctx, hc, origin+"/v1/dashboard/billing/subscription", token)
	if err != nil {
		return QuotaData{}, false, err
	}

	data := compatibleDataMap(subscription)
	if balance, ok := compatibleNumber(data["balance"]); ok {
		return compatibleWalletQuota("USD", balance, nil, nil, map[string]any{
			"source":      compatibleSourceOpenAIBilling,
			"balance_usd": nonNegativeFloat(balance),
		}), true, nil
	}

	hardLimit, hasHardLimit := compatibleNumber(data["hard_limit_usd"])
	if !hasHardLimit || hardLimit < 0 || hardLimit >= openAIBillingSentinelLimitUSD {
		return QuotaData{}, false, nil
	}

	now := time.Now().UTC()
	usageURL := fmt.Sprintf(
		"%s/v1/dashboard/billing/usage?start_date=%s-01-01&end_date=%s",
		origin,
		now.Format("2006"),
		now.Format("2006-01-02"),
	)
	usage, err := compatibleGetJSON(ctx, hc, usageURL, token)
	if err != nil {
		return QuotaData{}, false, err
	}

	usageData := compatibleDataMap(usage)
	usedUSD, hasUsed := compatibleNumber(usageData["used_usd"])
	if !hasUsed {
		if totalUsage, ok := compatibleNumber(usageData["total_usage"]); ok {
			usedUSD = totalUsage / 100
			hasUsed = true
		}
	}
	if !hasUsed {
		return QuotaData{}, false, errors.New("billing usage response did not contain a used amount")
	}

	remaining := math.Max(0, hardLimit-usedUSD)
	return compatibleWalletQuota("USD", remaining, &hardLimit, &usedUSD, map[string]any{
		"source":            compatibleSourceOpenAIBilling,
		"total_granted_usd": hardLimit,
		"total_used_usd":    usedUSD,
		"balance_usd":       remaining,
	}), true, nil
}

func queryDeepSeekQuota(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
	token string,
) (QuotaData, bool, error) {
	root, err := compatibleGetJSON(ctx, hc, origin+"/user/balance", token)
	if err != nil {
		return QuotaData{}, false, err
	}

	data := compatibleDataMap(root)
	balanceRows, ok := data["balance_infos"].([]any)
	if !ok || len(balanceRows) == 0 {
		return QuotaData{}, false, nil
	}

	balances := make([]map[string]any, 0, len(balanceRows))
	limits := make([]QuotaLimitStatus, 0, len(balanceRows))
	anyAvailable := false
	for _, rawRow := range balanceRows {
		row := compatibleMap(rawRow)
		if row == nil {
			continue
		}
		amount, hasAmount := compatibleNumber(row["total_balance"])
		if !hasAmount {
			continue
		}
		currency := strings.ToUpper(strings.TrimSpace(compatibleString(row["currency"])))
		if currency == "" {
			currency = "CNY"
		}
		balance := map[string]any{
			"amount":   nonNegativeFloat(amount),
			"currency": currency,
		}
		if granted, ok := compatibleNumber(row["granted_balance"]); ok {
			balance["granted_amount"] = granted
		}
		if toppedUp, ok := compatibleNumber(row["topped_up_balance"]); ok {
			balance["topped_up_amount"] = toppedUp
		}
		balances = append(balances, balance)

		status := "available"
		ratio := 0.0
		if amount <= 0 {
			status = "exhausted"
			ratio = 1
		} else {
			anyAvailable = true
		}
		limits = append(limits, NewTokenLimitStatus(status, ratio, nil).WithWindow(QuotaWindowCredits, 0))
	}

	if len(balances) == 0 {
		return QuotaData{}, false, nil
	}
	availableFlag, hasAvailableFlag := compatibleBoolOK(data["is_available"])
	if hasAvailableFlag && !availableFlag {
		anyAvailable = false
		for i := range limits {
			limits[i].Status = "exhausted"
			limits[i].UsageRatio = 1
			limits[i].Ready = false
		}
	}

	status := "exhausted"
	if anyAvailable {
		status = "available"
	}
	return QuotaData{
		Status:       status,
		ProviderType: "compatible",
		RawData: map[string]any{
			"source":   compatibleSourceDeepSeek,
			"balances": balances,
		},
		Ready:  IsReadyStatus(status),
		Limits: limits,
	}, true, nil
}

func compatibleWalletQuota(currency string, remaining float64, total, used *float64, rawData map[string]any) QuotaData {
	remaining = math.Max(0, remaining)
	status := "available"
	ratio := 0.0

	if total != nil && *total > 0 {
		usedValue := math.Max(0, *total-remaining)
		if used != nil {
			usedValue = math.Max(0, *used)
		}
		ratio = math.Min(1, usedValue / *total)
		switch {
		case remaining <= 0 || ratio >= 1:
			status = "exhausted"
			ratio = 1
		case ratio >= WarningThresholdRatio:
			status = "warning"
		}
	} else if remaining <= 0 {
		status = "exhausted"
		ratio = 1
	}

	limit := NewTokenLimitStatus(status, ratio, nil).WithWindow(QuotaWindowCredits, 0)
	if total != nil {
		periodQuota := *total
		limit.PeriodQuota = &periodQuota
	}
	if used != nil {
		periodCost := math.Max(0, *used)
		limit.PeriodCost = &periodCost
	}

	if rawData == nil {
		rawData = map[string]any{}
	}
	rawData["currency"] = currency
	rawData["remaining"] = remaining

	return QuotaData{
		Status:       status,
		ProviderType: "compatible",
		RawData:      rawData,
		Ready:        IsReadyStatus(status),
		Limits:       []QuotaLimitStatus{limit},
	}
}

func compatibleGetJSON(
	ctx context.Context,
	hc *httpclient.HttpClient,
	endpoint string,
	token string,
) (map[string]any, error) {
	request := httpclient.NewRequestBuilder().
		WithMethod(http.MethodGet).
		WithURL(endpoint).
		WithBearerToken(token).
		WithHeader("Accept", "application/json").
		Build()

	response, err := hc.Do(ctx, request)
	if err != nil {
		return nil, err
	}

	var root map[string]any
	if err := json.Unmarshal(response.Body, &root); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return root, nil
}

func compatibleMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func compatibleDataMap(root map[string]any) map[string]any {
	if data := compatibleMap(root["data"]); data != nil {
		return data
	}
	return root
}

func compatibleNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		result := float64(typed)
		return result, !math.IsNaN(result) && !math.IsInf(result, 0)
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		result, err := typed.Float64()
		return result, err == nil
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		result, err := strconv.ParseFloat(trimmed, 64)
		return result, err == nil && !math.IsNaN(result) && !math.IsInf(result, 0)
	default:
		return 0, false
	}
}

func firstCompatibleNumber(data map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := compatibleNumber(data[key]); ok {
			return value, true
		}
	}
	return 0, false
}

func compatibleBool(value any) bool {
	result, _ := compatibleBoolOK(value)
	return result
}

func compatibleBoolOK(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		result, err := strconv.ParseBool(strings.TrimSpace(typed))
		return result, err == nil
	default:
		return false, false
	}
}

func compatibleString(value any) string {
	result, _ := value.(string)
	return result
}

func compatibleTime(value any) *time.Time {
	number, ok := compatibleNumber(value)
	if !ok || number <= 0 {
		return nil
	}
	var result time.Time
	if number >= 1e12 {
		result = time.UnixMilli(int64(number))
	} else {
		result = time.Unix(int64(number), 0)
	}
	return &result
}

func nullableCompatibleNumber(value float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &value
}

func nonNegativeFloat(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	return value
}
