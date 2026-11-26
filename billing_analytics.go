//go:build windows

package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type BillingAnalyticsSummary struct {
	Month           string `json:"month"`
	SessionCount    int    `json:"session_count"`
	BilledAmount    int    `json:"billed_amount"`
	GameAmount      int    `json:"game_amount"`
	SnackAmount     int    `json:"snack_amount"`
	CollectedAmount int    `json:"collected_amount"`
	Outstanding     int    `json:"outstanding"`
}

type BillingAnalyticsDay struct {
	Date            string `json:"date"`
	SessionCount    int    `json:"session_count"`
	BilledAmount    int    `json:"billed_amount"`
	GameAmount      int    `json:"game_amount"`
	SnackAmount     int    `json:"snack_amount"`
	CollectedAmount int    `json:"collected_amount"`
}

type BillingAnalyticsWeek struct {
	Label           string `json:"label"`
	StartDate       string `json:"start_date"`
	EndDate         string `json:"end_date"`
	CollectedAmount int    `json:"collected_amount"`
}

type BillingAnalyticsCategory struct {
	Category             string `json:"category"`
	SessionCount         int    `json:"session_count"`
	StandaloneSnackSales int `json:"standalone_snack_sales"`
	Minutes              int    `json:"minutes"`
	GameAmount           int    `json:"game_amount"`
	SnackAmount          int    `json:"snack_amount"`
	BilledAmount         int    `json:"billed_amount"`
	CollectedAmount      int    `json:"collected_amount"`
	Outstanding          int    `json:"outstanding"`
}

type BillingAnalyticsPC struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type BillingAnalyticsReport struct {
	Month                 string                     `json:"month"`
	ThisMonth             BillingAnalyticsSummary    `json:"this_month"`
	LastMonth             BillingAnalyticsSummary    `json:"last_month"`
	SelectedMonth         BillingAnalyticsSummary    `json:"selected_month"`
	RangeStart            string                     `json:"range_start"`
	RangeEnd              string                     `json:"range_end"`
	RangeSummary          BillingAnalyticsSummary    `json:"range_summary"`
	AverageDailyCollected float64                    `json:"average_daily_collected"`
	Daily                 []BillingAnalyticsDay      `json:"daily"`
	Weekly                []BillingAnalyticsWeek     `json:"weekly"`
	Categories            []BillingAnalyticsCategory `json:"categories"`
	AvailableCategories   []string                   `json:"available_categories"`
	AvailablePCs          []BillingAnalyticsPC       `json:"available_pcs"`
	Complete              bool                       `json:"complete"`
	Devices               []BillingDeviceStatus      `json:"devices"`
}

func billingMonthStart(value string) (time.Time, error) {
	month, err := time.ParseInLocation("2006-01", value, billingTimezone)
	if err != nil || month.Format("2006-01") != value {
		return time.Time{}, fmt.Errorf("month must use YYYY-MM")
	}
	return time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, billingTimezone), nil
}

func billingDateStart(value string) (time.Time, error) {
	date, err := time.ParseInLocation(time.DateOnly, value, billingTimezone)
	if err != nil || date.Format(time.DateOnly) != value {
		return time.Time{}, fmt.Errorf("date must use YYYY-MM-DD")
	}
	return date, nil
}

func sessionPaymentEvents(session Session) []paymentRecord {
	events := append([]paymentRecord(nil), session.payments...)
	if len(events) == 0 {
		if session.AmountPaid == 0 {
			return nil
		}
		timestamp := session.PaymentAt
		if timestamp == "" {
			timestamp = session.StartedAt
		}
		return []paymentRecord{{Timestamp: timestamp, Amount: session.AmountPaid}}
	}

	recorded := 0
	for i := range events {
		recorded += events[i].Amount
		if events[i].Timestamp == "" {
			events[i].Timestamp = session.PaymentAt
			if events[i].Timestamp == "" {
				events[i].Timestamp = session.StartedAt
			}
		}
	}
	initial := session.AmountPaid - recorded
	if initial != 0 {
		events = append(events, paymentRecord{Timestamp: session.StartedAt, Amount: initial})
	}
	return events
}

func billingAnalyticsForMonth(sessions []Session, month time.Time) (BillingAnalyticsSummary, []BillingAnalyticsDay) {
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, billingTimezone)
	end := start.AddDate(0, 1, 0)
	summary, days := billingAnalyticsForRange(sessions, start, end)
	summary.Month = start.Format("2006-01")
	return summary, days
}

func billingAnalyticsForRange(sessions []Session, start, end time.Time) (BillingAnalyticsSummary, []BillingAnalyticsDay) {
	summary := BillingAnalyticsSummary{}
	dayCount := int(end.Sub(start) / (24 * time.Hour))
	if dayCount <= 0 {
		return summary, nil
	}
	days := make([]BillingAnalyticsDay, dayCount)
	dayIndices := make(map[string]int, len(days))
	for i := range days {
		date := start.AddDate(0, 0, i)
		key := date.Format(time.DateOnly)
		dayIndices[key] = i
		days[i].Date = key
	}

	for _, session := range sessions {
		started, err := sessionTimestamp(session.StartedAt)
		if err == nil && !started.Before(start) && started.Before(end) {
			standaloneSnack := isStandaloneSnackSale(session)
			if !standaloneSnack {
				summary.SessionCount++
			}
			summary.BilledAmount += session.FinalAmount
			if gameAmount := session.FinalAmount - session.Snacks; gameAmount > 0 {
				summary.GameAmount += gameAmount
			}
			summary.SnackAmount += session.Snacks
			if due := session.FinalAmount - session.AmountPaid; due > 0 {
				summary.Outstanding += due
			}
			day := dayIndices[started.In(billingTimezone).Format(time.DateOnly)]
			if !standaloneSnack {
				days[day].SessionCount++
			}
			days[day].BilledAmount += session.FinalAmount
			if gameAmount := session.FinalAmount - session.Snacks; gameAmount > 0 {
				days[day].GameAmount += gameAmount
			}
			days[day].SnackAmount += session.Snacks
		}

		for _, payment := range sessionPaymentEvents(session) {
			paidAt, err := sessionTimestamp(payment.Timestamp)
			if err != nil || paidAt.Before(start) || !paidAt.Before(end) {
				continue
			}
			summary.CollectedAmount += payment.Amount
			day := dayIndices[paidAt.In(billingTimezone).Format(time.DateOnly)]
			days[day].CollectedAmount += payment.Amount
		}
	}
	return summary, days
}

func billingAnalyticsSessionMinutes(session Session) int {
	if session.Status == "closed" {
		return (session.ActualDurationSeconds + 59) / 60
	}
	planned := session.PurchasedMinutes + session.AdditionalMinutes
	if session.Continued == nil {
		return planned
	}
	return (session.Continued.SpentSeconds+59)/60 + max(0, planned-session.Continued.PriorMinutes)
}

func billingAnalyticsMatches(session Session, category, pc, customer string) bool {
	if category != "" && !strings.EqualFold(strings.TrimSpace(session.Category), category) {
		return false
	}
	if pc != "" {
		if pc == "__unassigned__" {
			if strings.TrimSpace(session.PCID) != "" {
				return false
			}
		} else if !strings.EqualFold(session.PCID, pc) && !strings.EqualFold(session.PCName, pc) {
			return false
		}
	}
	return customer == "" || strings.Contains(strings.ToLower(session.CustomerName), customer)
}

func billingAnalyticsAvailableFilters(sessions []Session) ([]string, []BillingAnalyticsPC) {
	categories := make(map[string]struct{})
	pcs := make(map[string]string)
	for _, session := range sessions {
		if session.Status == "removed" {
			continue
		}
		if category := strings.TrimSpace(session.Category); category != "" {
			categories[category] = struct{}{}
		}
		name := strings.TrimSpace(session.PCName)
		if name == "" {
			name = "Unassigned"
		}
		id := strings.TrimSpace(session.PCID)
		if id == "" {
			id = "__unassigned__"
		}
		pcs[id] = name
	}
	categoryList := make([]string, 0, len(categories))
	for category := range categories {
		categoryList = append(categoryList, category)
	}
	sort.Strings(categoryList)
	pcList := make([]BillingAnalyticsPC, 0, len(pcs))
	for id, name := range pcs {
		pcList = append(pcList, BillingAnalyticsPC{ID: id, Name: name})
	}
	sort.Slice(pcList, func(i, j int) bool {
		if pcList[i].Name == pcList[j].Name {
			return pcList[i].ID < pcList[j].ID
		}
		return pcList[i].Name < pcList[j].Name
	})
	return categoryList, pcList
}

func billingAnalyticsCategories(sessions []Session, start, end time.Time) []BillingAnalyticsCategory {
	byCategory := make(map[string]*BillingAnalyticsCategory)
	for _, session := range sessions {
		category := strings.TrimSpace(session.Category)
		if category == "" {
			category = "Uncategorized"
		}
		started, startErr := sessionTimestamp(session.StartedAt)
		startedInRange := startErr == nil && !started.Before(start) && started.Before(end)
		payments := sessionPaymentEvents(session)
		collected := 0
		for _, payment := range payments {
			paidAt, err := sessionTimestamp(payment.Timestamp)
			if err == nil && !paidAt.Before(start) && paidAt.Before(end) {
				collected += payment.Amount
			}
		}
		if !startedInRange && collected == 0 {
			continue
		}
		item := byCategory[category]
		if item == nil {
			item = &BillingAnalyticsCategory{Category: category}
			byCategory[category] = item
		}
		item.CollectedAmount += collected
		if !startedInRange {
			continue
		}
		item.BilledAmount += session.FinalAmount
		item.GameAmount += max(0, session.FinalAmount-session.Snacks)
		item.SnackAmount += session.Snacks
		if due := session.FinalAmount - session.AmountPaid; due > 0 {
			item.Outstanding += due
		}
		if isStandaloneSnackSale(session) {
			item.StandaloneSnackSales++
		} else {
			item.SessionCount++
			item.Minutes += billingAnalyticsSessionMinutes(session)
		}
	}
	result := make([]BillingAnalyticsCategory, 0, len(byCategory))
	for _, item := range byCategory {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Category < result[j].Category })
	return result
}

func billingAnalyticsWeeks(days []BillingAnalyticsDay) []BillingAnalyticsWeek {
	if len(days) == 0 {
		return nil
	}
	var weeks []BillingAnalyticsWeek
	var currentStart time.Time
	rangeStart, _ := time.ParseInLocation(time.DateOnly, days[0].Date, billingTimezone)
	rangeEnd, _ := time.ParseInLocation(time.DateOnly, days[len(days)-1].Date, billingTimezone)
	for _, day := range days {
		date, err := time.ParseInLocation(time.DateOnly, day.Date, billingTimezone)
		if err != nil {
			continue
		}
		weekdayOffset := (int(date.Weekday()) + 6) % 7
		weekStart := date.AddDate(0, 0, -weekdayOffset)
		if len(weeks) == 0 || !weekStart.Equal(currentStart) {
			currentStart = weekStart
			start := currentStart
			if start.Before(rangeStart) {
				start = rangeStart
			}
			end := currentStart.AddDate(0, 0, 6)
			if end.After(rangeEnd) {
				end = rangeEnd
			}
			weeks = append(weeks, BillingAnalyticsWeek{
				StartDate: start.Format(time.DateOnly),
				EndDate:   end.Format(time.DateOnly),
				Label:     fmt.Sprintf("%s-%s", start.Format("Jan 2"), end.Format("Jan 2")),
			})
		}
		weeks[len(weeks)-1].CollectedAmount += day.CollectedAmount
	}
	return weeks
}

func billingAnalyticsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
		return
	}
	monthValue := r.URL.Query().Get("month")
	if monthValue == "" {
		monthValue = time.Now().In(billingTimezone).Format("2006-01")
	}
	selectedStart, err := billingMonthStart(monthValue)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rangeStart, rangeEnd := selectedStart, selectedStart.AddDate(0, 1, 0)
	startValue, endValue := r.URL.Query().Get("start"), r.URL.Query().Get("end")
	if (startValue == "") != (endValue == "") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start and end dates must be provided together"})
		return
	}
	if startValue != "" {
		rangeStart, err = billingDateStart(startValue)
		if err == nil {
			inclusiveEnd, endErr := billingDateStart(endValue)
			if endErr != nil {
				err = endErr
			} else if inclusiveEnd.Before(rangeStart) {
				err = fmt.Errorf("end date must be on or after start date")
			} else {
				rangeEnd = inclusiveEnd.AddDate(0, 0, 1)
				if rangeEnd.Sub(rangeStart) > 3660*24*time.Hour {
					err = fmt.Errorf("date range cannot exceed 10 years")
				}
			}
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	sessions, err := billingStore.sessions("", "")
	if err != nil {
		writeBillingError(w, err)
		return
	}
	availableCategories, availablePCs := billingAnalyticsAvailableFilters(sessions)
	categoryFilter := strings.TrimSpace(r.URL.Query().Get("category"))
	pcFilter := strings.TrimSpace(r.URL.Query().Get("pc"))
	customerFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("customer")))
	filteredSessions := make([]Session, 0, len(sessions))
	for _, session := range sessions {
		if billingAnalyticsMatches(session, categoryFilter, pcFilter, customerFilter) {
			filteredSessions = append(filteredSessions, session)
		}
	}

	now := time.Now().In(billingTimezone)
	thisStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, billingTimezone)
	lastStart := thisStart.AddDate(0, -1, 0)
	thisMonth, _ := billingAnalyticsForMonth(filteredSessions, thisStart)
	lastMonth, _ := billingAnalyticsForMonth(filteredSessions, lastStart)
	selected, _ := billingAnalyticsForMonth(filteredSessions, selectedStart)
	rangeSummary, rangeDaily := billingAnalyticsForRange(filteredSessions, rangeStart, rangeEnd)
	categories := billingAnalyticsCategories(filteredSessions, rangeStart, rangeEnd)
	averageDaily := float64(0)
	if len(rangeDaily) > 0 {
		averageDaily = float64(rangeSummary.CollectedAmount) / float64(len(rangeDaily))
	}
	devices := billingStore.billingSyncStatuses()
	complete := len(devices) > 0
	for _, device := range devices {
		if !device.Online || device.PendingRecords > 0 || device.Error != "" {
			complete = false
		}
	}

	writeJSON(w, http.StatusOK, BillingAnalyticsReport{
		Month:                selectedStart.Format("2006-01"),
		ThisMonth:            thisMonth,
		LastMonth:            lastMonth,
		SelectedMonth:        selected,
		RangeStart:           rangeStart.Format(time.DateOnly),
		RangeEnd:             rangeEnd.AddDate(0, 0, -1).Format(time.DateOnly),
		RangeSummary:          rangeSummary,
		AverageDailyCollected: averageDaily,
		Daily:                rangeDaily,
		Weekly:               billingAnalyticsWeeks(rangeDaily),
		Categories:    categories,
		AvailableCategories: availableCategories,
		AvailablePCs:  availablePCs,
		Complete:             complete,
		Devices:               devices,
	})
}
