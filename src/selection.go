package main

import (
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type authID string

func rankedAccounts(req pluginapi.SchedulerPickRequest, quotas map[authID]quotaSnapshot, now time.Time, ttl time.Duration) []string {
	var five, weekly []string
	var firstFive, firstWeek time.Time
	for _, candidate := range req.Candidates {
		q, exists := quotas[authID(candidate.ID)]
		if !exists {
			continue
		}
		fiveReset, weekReset, usable := q.usable(req.Model, now, ttl)
		if !usable {
			continue
		}
		if !fiveReset.IsZero() {
			if firstFive.IsZero() || fiveReset.Before(firstFive) {
				firstFive, five = fiveReset, nil
			}
			if fiveReset.Equal(firstFive) {
				five = append(five, candidate.ID)
			}
		}
		if !weekReset.IsZero() {
			if firstWeek.IsZero() || weekReset.Before(firstWeek) {
				firstWeek, weekly = weekReset, nil
			}
			if weekReset.Equal(firstWeek) {
				weekly = append(weekly, candidate.ID)
			}
		}
	}
	if len(five) > 0 {
		sort.Strings(five)
		return five
	}
	sort.Strings(weekly)
	return weekly
}
