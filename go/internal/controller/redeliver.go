package controller

import (
	"context"
	"time"

	"github.com/RooobinYe/ai-pr-review-cloud/go/internal/githubapp"
)

// GitHub does not retry a webhook the gateway failed to accept. The controller
// periodically redelivers such deliveries; the gateway deduplicates them by
// delivery ID, so a redelivery that crosses a late success is harmless.
const (
	// redeliveryWindow is how far back failed deliveries are redelivered.
	redeliveryWindow = 24 * time.Hour
	// maxDeliveryAttempts counts the original delivery and its redeliveries.
	maxDeliveryAttempts = 5
)

// Redeliveries picks the deliveries to send again: for every delivery GUID in
// the window whose attempts all failed, the newest attempt, as long as the
// GUID has had fewer than maxDeliveryAttempts.
func Redeliveries(ds []githubapp.Delivery, now time.Time) []int64 {
	type guid struct {
		attempts  int
		delivered bool
		newest    githubapp.Delivery
	}
	byGUID := map[string]*guid{}
	var order []string
	for _, d := range ds {
		g, ok := byGUID[d.GUID]
		if !ok {
			g = &guid{newest: d}
			byGUID[d.GUID] = g
			order = append(order, d.GUID)
		}
		g.attempts++
		g.delivered = g.delivered || d.StatusCode/100 == 2
		if d.DeliveredAt.After(g.newest.DeliveredAt) {
			g.newest = d
		}
	}
	var ids []int64
	for _, id := range order {
		g := byGUID[id]
		if g.delivered || g.attempts >= maxDeliveryAttempts || now.Sub(g.newest.DeliveredAt) > redeliveryWindow {
			continue
		}
		ids = append(ids, g.newest.ID)
	}
	return ids
}

// Redeliver sends every failed recent webhook delivery again.
func (c *Controller) Redeliver(ctx context.Context) {
	ds, err := c.gh.ListDeliveries(ctx)
	if err != nil {
		c.log.Error("list webhook deliveries", "err", err)
		return
	}
	for _, id := range Redeliveries(ds, c.now()) {
		if err := c.gh.Redeliver(ctx, id); err != nil {
			c.log.Error("redeliver webhook", "delivery_id", id, "err", err)
			continue
		}
		c.log.Info("redelivered failed webhook", "delivery_id", id)
	}
}

// RedeliverEvery runs Redeliver every interval until ctx is done.
func (c *Controller) RedeliverEvery(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Redeliver(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
