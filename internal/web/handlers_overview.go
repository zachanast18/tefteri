package web

import (
	"net/http"
	"time"

	"spending-project/internal/db"
	"spending-project/internal/money"
	"spending-project/internal/recurrence"
)

type categoryRow struct {
	CategoryID     int64
	CategoryName   string
	TotalFormatted string
	SharePercent   float64
	BudgetText     string
	BudgetOver     bool
}

type subReminderRow struct {
	db.Subscription
	DueLabel   string
	IsWithin7  bool
	IsWithin30 bool
}

type trendRow struct {
	Month          string
	MonthLabel     string
	TotalFormatted string
	BarPercent     float64
}

type overviewPageData struct {
	BasePage
	PeriodTitle            string
	PeriodSubtitle         string
	FromDate               string
	ToDate                 string
	PrevMonth              string
	CurrentMonth           string
	NextMonth              string
	TotalFormatted         string
	CashFormatted          string
	CardFormatted          string
	Summary                db.DashboardSummary
	CategoryRows           []categoryRow
	Categories             []db.Category
	TrendRows              []trendRow
	UpcomingSubs           []subReminderRow
	DueSoonCount           int
	EstMonthlySubFormatted string
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	u, _ := s.currentUser(r)
	hhID := u.ActiveHouseholdID
	now := s.nowFn().In(s.cfg.Location)
	currentMonth := now.Format("2006-01")
	today := recurrence.Today(now, s.cfg.Location)

	q := r.URL.Query()
	fromDate := q.Get("from")
	toDate := q.Get("to")
	monthParam := q.Get("month")

	var periodTitle, periodSubtitle, trendEndMonth string
	var targetMonth time.Time

	if fromDate != "" && toDate != "" {
		fT, err1 := recurrence.ParseDate(fromDate, s.cfg.Location)
		tT, err2 := recurrence.ParseDate(toDate, s.cfg.Location)
		if err1 == nil && err2 == nil && !tT.Before(fT) {
			periodTitle = "Custom range"
			periodSubtitle = fT.Format("02/01/2006") + " – " + tT.Format("02/01/2006")
			trendEndMonth = tT.Format("2006-01")
			targetMonth = time.Date(tT.Year(), tT.Month(), 1, 0, 0, 0, 0, s.cfg.Location)
		} else {
			fromDate, toDate = "", ""
		}
	}

	if fromDate == "" || toDate == "" {
		if monthParam != "" {
			if m, err := time.ParseInLocation("2006-01", monthParam, s.cfg.Location); err == nil {
				targetMonth = m
			}
		}
		if targetMonth.IsZero() {
			targetMonth = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, s.cfg.Location)
		}
		firstDay := time.Date(targetMonth.Year(), targetMonth.Month(), 1, 0, 0, 0, 0, s.cfg.Location)
		lastDay := time.Date(targetMonth.Year(), targetMonth.Month()+1, 0, 0, 0, 0, 0, s.cfg.Location)
		fromDate = firstDay.Format("2006-01-02")
		toDate = lastDay.Format("2006-01-02")
		periodTitle = targetMonth.Format("January 2006")
		periodSubtitle = "Confirmed spending"
		trendEndMonth = targetMonth.Format("2006-01")
	}

	prevM := time.Date(targetMonth.Year(), targetMonth.Month()-1, 1, 0, 0, 0, 0, s.cfg.Location).Format("2006-01")
	nextM := time.Date(targetMonth.Year(), targetMonth.Month()+1, 1, 0, 0, 0, 0, s.cfg.Location).Format("2006-01")

	summary, err := s.store.GetDashboardSummary(r.Context(), hhID, fromDate, toDate, trendEndMonth, s.cfg.Location)
	if err != nil {
		http.Error(w, "failed to load overview", http.StatusInternalServerError)
		return
	}

	budgets, _ := s.store.ListCategoryBudgets(r.Context(), hhID)
	allCats, _ := s.store.ListCategories(r.Context(), hhID)

	catRows := make([]categoryRow, 0, len(summary.ByCategory))
	for _, c := range summary.ByCategory {
		row := categoryRow{
			CategoryID:     c.CategoryID,
			CategoryName:   c.CategoryName,
			TotalFormatted: money.Format(c.TotalCents, s.cfg.CurrencySymbol),
			SharePercent:   c.SharePercent,
		}
		if lim, ok := budgets[c.CategoryID]; ok && lim > 0 {
			diff := lim - c.TotalCents
			if diff >= 0 {
				row.BudgetText = money.Format(diff, s.cfg.CurrencySymbol) + " left of " + money.Format(lim, s.cfg.CurrencySymbol)
			} else {
				row.BudgetOver = true
				row.BudgetText = "Over by " + money.Format(-diff, s.cfg.CurrencySymbol) + " (" + money.Format(lim, s.cfg.CurrencySymbol) + " budget)"
			}
		}
		catRows = append(catRows, row)
	}

	var maxTrend int64
	for _, pt := range summary.MonthlyTrend {
		if pt.TotalCents > maxTrend {
			maxTrend = pt.TotalCents
		}
	}
	trendRows := make([]trendRow, 0, len(summary.MonthlyTrend))
	for _, pt := range summary.MonthlyTrend {
		pct := 0.0
		if maxTrend > 0 && pt.TotalCents > 0 {
			pct = float64(pt.TotalCents) * 100.0 / float64(maxTrend)
		}
		label := pt.Month
		if t, err := time.Parse("2006-01", pt.Month); err == nil {
			label = t.Format("Jan 06")
		}
		trendRows = append(trendRows, trendRow{
			Month:          pt.Month,
			MonthLabel:     label,
			TotalFormatted: money.Format(pt.TotalCents, s.cfg.CurrencySymbol),
			BarPercent:     pct,
		})
	}

	allSubs, err := s.store.ListSubscriptions(r.Context(), hhID, today)
	if err != nil {
		http.Error(w, "failed to load subscriptions", http.StatusInternalServerError)
		return
	}
	todayT, _ := recurrence.ParseDate(today, s.cfg.Location)
	var upcoming []subReminderRow
	dueSoonCount := 0
	for _, sub := range allSubs {
		if sub.Status == "active" {
			row := subReminderRow{Subscription: sub}
			if dueT, err := recurrence.ParseDate(sub.NextDueDate, s.cfg.Location); err == nil {
				days := int(dueT.Sub(todayT).Hours() / 24)
				switch {
				case days < 0:
					row.DueLabel = "Overdue"
					row.IsWithin7 = true
					dueSoonCount++
				case days == 0:
					row.DueLabel = "Due today"
					row.IsWithin7 = true
					dueSoonCount++
				case days <= 7:
					row.DueLabel = "Due in " + strconvItoa(days) + "d"
					row.IsWithin7 = true
					dueSoonCount++
				case days <= 30:
					row.DueLabel = "Due in " + strconvItoa(days) + "d"
					row.IsWithin30 = true
				}
			}
			upcoming = append(upcoming, row)
			if len(upcoming) >= 5 {
				break
			}
		}
	}

	estMonthly, err := s.store.EstimatedMonthlySubscriptionCost(r.Context(), hhID)
	if err != nil {
		estMonthly = 0
	}

	s.render(w, r, "overview.html", http.StatusOK, overviewPageData{
		BasePage:               s.newBasePage(w, r, "Overview", "overview", true),
		PeriodTitle:            periodTitle,
		PeriodSubtitle:         periodSubtitle,
		FromDate:               fromDate,
		ToDate:                 toDate,
		PrevMonth:              prevM,
		CurrentMonth:           currentMonth,
		NextMonth:              nextM,
		TotalFormatted:         money.Format(summary.TotalCents, s.cfg.CurrencySymbol),
		CashFormatted:          money.Format(summary.CashCents, s.cfg.CurrencySymbol),
		CardFormatted:          money.Format(summary.CardCents, s.cfg.CurrencySymbol),
		Summary:                summary,
		CategoryRows:           catRows,
		Categories:             allCats,
		TrendRows:              trendRows,
		UpcomingSubs:           upcoming,
		DueSoonCount:           dueSoonCount,
		EstMonthlySubFormatted: money.Format(estMonthly, s.cfg.CurrencySymbol),
	})
}

func strconvItoa(n int) string {
	return time.Duration(n).String()[:0] + func() string {
		if n == 0 {
			return "0"
		}
		buf := [12]byte{}
		i := len(buf)
		for n > 0 {
			i--
			buf[i] = byte('0' + n%10)
			n /= 10
		}
		return string(buf[i:])
	}()
}
