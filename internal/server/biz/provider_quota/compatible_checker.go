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
		{source: compatibleSourceSub2API, query: querySub2APIAccountQuota},
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

// CheckStandaloneBalance probes only the supported self-hosted gateway APIs.
// It intentionally does not inspect a channel type or provider registration.
func (c *CompatibleQuotaChecker) CheckStandaloneBalance(ctx context.Context, baseURL, token string) (QuotaData, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return QuotaData{}, errors.New("balance token is required")
	}

	origin, err := compatibleQuotaOrigin(baseURL)
	if err != nil {
		return QuotaData{}, err
	}

	probes := []struct {
		source string
		query  func(context.Context, *httpclient.HttpClient, string, string) (QuotaData, bool, error)
	}{
		{source: compatibleSourceNewAPI, query: queryNewAPIQuota},
		{source: compatibleSourceSub2API, query: querySub2APIAccountQuota},
	}

	var failures []string
	for _, probe := range probes {
		data, recognized, probeErr := probe.query(ctx, c.httpClient, origin, token)
		if recognized {
			return NormalizeQuotaData(data), nil
		}
		if probeErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", probe.source, probeErr))
		}
	}

	if len(failures) == 0 {
		return QuotaData{}, errors.New("no New API or Sub2API balance endpoint returned usable data")
	}
	return QuotaData{}, fmt.Errorf("no New API or Sub2API balance endpoint returned usable data (%s)", strings.Join(failures, "; "))
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
		return queryNewAPIAccountQuota(ctx, hc, origin, token, err)
	}

	data := compatibleDataMap(root)
	totalGranted, hasGranted := compatibleNumber(data["total_granted"])
	totalUsed, hasUsed := compatibleNumber(data["total_used"])
	totalAvailable, hasAvailable := compatibleNumber(data["total_available"])
	unlimited := compatibleBool(data["unlimited_quota"]) || (hasGranted && totalGranted < 0)
	if !unlimited && !hasGranted && !hasUsed && !hasAvailable {
		return queryNewAPIAccountQuota(ctx, hc, origin, token, nil)
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

// queryNewAPIAccountQuota supports the account endpoint used by current New API
// deployments. Unlike /api/usage/token/, /api/user/self returns the user's
// remaining quota in the site's integer quota unit. `quota` is already the
// remaining amount; `used_quota` is cumulative usage and must not be subtracted
// from it a second time.
func queryNewAPIAccountQuota(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
	token string,
	usageErr error,
) (QuotaData, bool, error) {
	root, err := compatibleGetJSON(ctx, hc, origin+"/api/user/self", token)
	if err != nil {
		if usageErr != nil {
			return QuotaData{}, false, fmt.Errorf("token usage endpoint: %w; account endpoint: %v", usageErr, err)
		}
		return QuotaData{}, false, err
	}

	data := compatibleDataMap(root)
	quota, hasQuota := compatibleNumber(data["quota"])
	if !hasQuota {
		return QuotaData{}, false, nil
	}

	rawData := map[string]any{
		"source":          compatibleSourceNewAPI,
		"quota_remaining": quota,
	}
	if usedQuota, ok := compatibleNumber(data["used_quota"]); ok {
		rawData["quota_used"] = usedQuota
	}

	quotaPerUnit, displayCurrency, usdPerDisplayUnit := queryNewAPICurrencyBasis(ctx, hc, origin)
	if quotaPerUnit <= 0 {
		quotaPerUnit = compatibleQuotaPerUSD
	}

	if quota < 0 {
		rawData["unlimited_quota"] = true
		rawData["quota_per_unit"] = quotaPerUnit
		return QuotaData{
			Status:       "available",
			ProviderType: "compatible",
			RawData:      rawData,
			Ready:        true,
		}, true, nil
	}

	if displayCurrency != "" {
		rawData["display_currency"] = displayCurrency
	}
	rawData["quota_per_unit"] = quotaPerUnit
	remainingDisplayUnits := math.Max(0, quota/float64(quotaPerUnit))
	remainingUSD := remainingDisplayUnits * usdPerDisplayUnit
	rawData["balance_site_units"] = remainingDisplayUnits
	rawData["balance_usd"] = nonNegativeFloat(remainingUSD)
	rawData["total_available_usd"] = nonNegativeFloat(remainingUSD)

	return compatibleWalletQuota("USD", remainingUSD, nil, nil, rawData), true, nil
}

// queryNewAPICurrencyBasis reads the public New API status document. New API
// stores quota in an integer unit and publishes how many of those units make
// one displayed currency unit. The returned rate converts one displayed unit
// to USD, so CNY sites with usd_exchange_rate=7.3 use 1/7.3.
func queryNewAPICurrencyBasis(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
) (int64, string, float64) {
	// status is intentionally public, so it is queried without credentials.
	root, err := compatibleGetJSON(ctx, hc, origin+"/api/status", "")
	if err != nil {
		return compatibleQuotaPerUSD, "USD", 1
	}

	data := compatibleDataMap(root)
	quotaPerUnitFloat, hasQuotaPerUnit := compatibleNumber(data["quota_per_unit"])
	quotaPerUnit := int64(quotaPerUnitFloat)
	if !hasQuotaPerUnit || quotaPerUnit <= 0 {
		quotaPerUnit = compatibleQuotaPerUSD
	}

	currency := strings.ToUpper(strings.TrimSpace(compatibleString(data["quota_display_type"])))
	if currency == "" {
		currency = "USD"
	}

	rate := 1.0
	switch currency {
	case "CNY", "RMB":
		if value, ok := compatibleNumber(data["usd_exchange_rate"]); ok && value > 0 {
			rate = 1 / value
		}
	case "USD":
		// Keep the default rate.
	case "CUSTOM":
		if value, ok := compatibleNumber(data["custom_currency_exchange_rate"]); ok && value > 0 {
			rate = 1 / value
		}
	default:
		// Unknown display currencies have no reliable USD conversion metadata.
		// Keep the quota-per-unit default rather than inventing a rate.
	}

	return quotaPerUnit, currency, rate
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

func querySub2APIAccountQuota(
	ctx context.Context,
	hc *httpclient.HttpClient,
	origin string,
	token string,
) (QuotaData, bool, error) {
	root, err := compatibleGetJSON(ctx, hc, origin+"/api/v1/auth/me", token)
	if err != nil {
		return QuotaData{}, false, err
	}

	data := compatibleDataMap(root)
	balance, ok := compatibleNumber(data["balance"])
	if !ok {
		return QuotaData{}, false, nil
	}

	return compatibleWalletQuota("USD", balance, nil, nil, map[string]any{
		"source":      compatibleSourceSub2API,
		"balance_usd": nonNegativeFloat(balance),
	}), true, nil
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
	builder := httpclient.NewRequestBuilder().
		WithMethod(http.MethodGet).
		WithURL(endpoint).
		WithHeader("Accept", "application/json")
	if strings.TrimSpace(token) != "" {
		builder.WithBearerToken(token)
	}
	request := builder.Build()

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
