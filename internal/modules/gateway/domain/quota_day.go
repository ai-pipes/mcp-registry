package domain

import "time"

const quotaDayLayout = "2006-01-02"

// QuotaDayOf is the day a daily limit counts in: the UTC calendar day (YYYY-MM-DD), whatever
// the time zone of now; resetAt is the next UTC midnight.
func QuotaDayOf(now time.Time) (day string, resetAt time.Time) {
	u := now.UTC()
	day = u.Format(quotaDayLayout)
	return day, time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

// QuotaResetAt is the UTC midnight that ends a stored day (YYYY-MM-DD); an error for text that
// is not a day.
func QuotaResetAt(day string) (time.Time, error) {
	d, err := time.Parse(quotaDayLayout, day)
	if err != nil {
		return time.Time{}, err
	}
	return d.AddDate(0, 0, 1), nil
}

// QuotaOldestKept is the oldest day whose counter is kept: yesterday (UTC).
func QuotaOldestKept(now time.Time) string {
	return now.UTC().Add(-24 * time.Hour).Format(quotaDayLayout)
}
