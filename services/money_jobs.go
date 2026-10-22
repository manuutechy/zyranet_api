package services

import (
	"log"
	"time"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// MoneyJobs runs the background work that keeps payments honest when a
// callback never comes:
//
//   - every minute, STK pushes still "pending" after 2 minutes are checked with
//     Safaricom's STK query and completed or failed through the same paths a
//     callback uses (up to 30 minutes old; after that they're failed with a
//     clear reason — a late real callback still completes them);
//   - every minute, paybill confirmations whose verification never came back
//     are parked for manual review;
//   - every 15 seconds, queued router actions whose retry time has come run.
type MoneyJobs struct {
	mpesa    *MpesaService
	stopChan chan struct{}
}

// Pending STK pushes are reconciled in this age window.
const (
	stkReconcileAfter = 2 * time.Minute
	stkReconcileUntil = 30 * time.Minute
)

// NewMoneyJobs builds the job runner.
func NewMoneyJobs(mpesa *MpesaService) *MoneyJobs {
	return &MoneyJobs{mpesa: mpesa, stopChan: make(chan struct{})}
}

// Start launches the background loops.
func (j *MoneyJobs) Start() {
	minute := time.NewTicker(60 * time.Second)
	router := time.NewTicker(15 * time.Second)
	log.Println("[money-jobs] Pending-payment reconciler and router retry queue started.")
	go func() {
		for {
			select {
			case <-minute.C:
				j.ReconcilePendingSTK()
				ParkStaleC2BVerifications(10 * time.Minute)
			case <-router.C:
				j.mpesa.RunDueRouterActions()
			case <-j.stopChan:
				minute.Stop()
				router.Stop()
				log.Println("[money-jobs] stopped.")
				return
			}
		}
	}()
}

// Stop terminates the loops.
func (j *MoneyJobs) Stop() {
	close(j.stopChan)
}

// ReconcilePendingSTK asks Safaricom about STK pushes nobody has heard back
// on. Exported for tests.
func (j *MoneyJobs) ReconcilePendingSTK() {
	now := time.Now()
	// A little past the 30-minute limit so a payment that turned 30 between
	// two runs still gets its final check and closing failure.
	var pending []models.Payment
	if err := config.DB.
		Where("method = ? AND status = ? AND mpesa_transaction_id IS NOT NULL AND mpesa_transaction_id <> '' AND created_at <= ? AND created_at >= ?",
			"mpesa", "pending", now.Add(-stkReconcileAfter), now.Add(-stkReconcileUntil-5*time.Minute)).
		Order("id ASC").Limit(100).Find(&pending).Error; err != nil {
		log.Printf("[money-jobs] could not load pending payments: %v", err)
		return
	}
	for i := range pending {
		p := &pending[i]
		status, err := j.mpesa.QueryAndUpdateSTKStatus(p)
		if err != nil {
			log.Printf("[money-jobs] payment %d: status query failed: %v", p.ID, err)
		}
		if status == "pending" && now.Sub(p.CreatedAt) >= stkReconcileUntil {
			// Safaricom still hasn't given a final answer after 30 minutes.
			if err := j.mpesa.ProcessPaymentFailure(p, STKUnconfirmedReason); err == nil {
				log.Printf("[money-jobs] payment %d: no final answer from Safaricom after 30 minutes — marked failed (a late success still completes it)", p.ID)
			}
		} else if status != "pending" {
			log.Printf("[money-jobs] payment %d resolved by status query: %s", p.ID, status)
		}
		time.Sleep(200 * time.Millisecond) // stay well inside Daraja's rate limits
	}
}
