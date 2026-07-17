package main

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

func startBackgroundSync(interval time.Duration) {
	ticker := time.NewTicker(interval)
	// Run once immediately on start
	syncPendingAndInvalidate()

	for range ticker.C {
		syncPendingAndInvalidate()
	}
}

func syncPendingAndInvalidate() {
	log.Println("Starting background sync for pending tasks...")
	// 1. Find all pending records that have a vendor and a task ID
	rows, err := db.Query(`
		SELECT r.id, o.card_secret, COALESCE(sk.vendor_key, o.card_secret) AS vendor_key, r.task_id, o.vendor
		FROM account_records r
		JOIN orders o ON r.order_id = o.id
		LEFT JOIN system_keys sk ON o.card_secret = sk.system_key
		WHERE r.status NOT IN ('success', 'failed') AND o.vendor = 'pass.aisale.one' AND r.task_id != ''`)
	if err != nil {
		log.Printf("Background sync: failed to query pending records: %v\n", err)
		return
	}
	defer rows.Close()

	type PendingRecord struct {
		ID         int64
		CardSecret string
		VendorKey  string
		TaskID     string
		Vendor     string
	}

	var pendingList []PendingRecord
	for rows.Next() {
		var pr PendingRecord
		if err := rows.Scan(&pr.ID, &pr.CardSecret, &pr.VendorKey, &pr.TaskID, &pr.Vendor); err != nil {
			log.Printf("Background sync: failed to scan pending record: %v\n", err)
			continue
		}
		pendingList = append(pendingList, pr)
	}
	rows.Close()

	if len(pendingList) > 0 {
		var wg sync.WaitGroup
		for _, pr := range pendingList {
			wg.Add(1)
			go func(record PendingRecord) {
				defer wg.Done()
				res, err := queryTaskFromVendor(record.VendorKey, record.TaskID)
				if err != nil {
					log.Printf("Background sync: error querying vendor for task %s: %v\n", record.TaskID, err)
					return
				}
				if !res.Success {
					log.Printf("Background sync: vendor returned error status for task %s: %s\n", record.TaskID, res.Message)
					return
				}

				vendorStatus := res.Data.Status
				var localStatus string
				var completedAt *time.Time
				now := time.Now()

				switch strings.ToLower(vendorStatus) {
				case "success":
					localStatus = "success"
					completedAt = &now
				case "failed":
					localStatus = "failed"
					completedAt = &now
				case "running":
					localStatus = "running"
				case "pending":
					localStatus = "pending"
				default:
					localStatus = "pending"
				}

				discountURL := ""
				if res.Data.HasOfferURL && res.Data.OfferURL != "" {
					discountURL = res.Data.OfferURL
				}

				message := res.Data.Message
				if message == "" {
					message = "处理中"
				}

				var errUpdate error
				if completedAt != nil {
					_, errUpdate = db.Exec(`
						UPDATE account_records 
						SET status = ?, message = ?, discount_url = ?, completed_at = ?, updated_at = ? 
						WHERE id = ?`,
						localStatus, message, discountURL, *completedAt, now, record.ID)
				} else {
					_, errUpdate = db.Exec(`
						UPDATE account_records 
						SET status = ?, message = ?, discount_url = ?, updated_at = ? 
						WHERE id = ?`,
						localStatus, message, discountURL, now, record.ID)
				}

				if errUpdate != nil {
					log.Printf("Background sync: failed to update database for task %s: %v\n", record.TaskID, errUpdate)
				}
			}(pr)
		}
		wg.Wait()
	}

	// 2. Invalidate keys for any active system keys whose orders have successful subscriptions
	now := time.Now()
	res, err := db.Exec(`
		UPDATE system_keys sk
		JOIN orders o ON sk.system_key = o.card_secret
		JOIN account_records r ON r.order_id = o.id
		SET sk.status = 'inactive', sk.updated_at = ?
		WHERE sk.status = 'active' AND r.status = 'success'`, now)
	if err != nil {
		log.Printf("Background sync: failed to invalidate system keys: %v\n", err)
	} else {
		rowsAffected, _ := res.RowsAffected()
		if rowsAffected > 0 {
			log.Printf("Background sync: successfully invalidated %d active system keys due to successful subscriptions\n", rowsAffected)
		}
	}

	// 3. Expire pending key orders older than 5 minutes and release pre-locked stock
	expireTime := now.Add(-5 * time.Minute)
	rowsExpired, errExpired := db.Query("SELECT id, out_trade_no FROM key_orders WHERE status = 'pending' AND created_at < ?", expireTime)
	if errExpired == nil {
		defer rowsExpired.Close()
		type ExpiredOrder struct {
			ID         int64
			OutTradeNo string
		}
		var expiredList []ExpiredOrder
		for rowsExpired.Next() {
			var eo ExpiredOrder
			if errScan := rowsExpired.Scan(&eo.ID, &eo.OutTradeNo); errScan == nil {
				expiredList = append(expiredList, eo)
			}
		}
		rowsExpired.Close()

		for _, eo := range expiredList {
			tx, errTx := db.Begin()
			if errTx != nil {
				continue
			}
			var currentStatus string
			errRow := tx.QueryRow("SELECT status FROM key_orders WHERE id = ? FOR UPDATE", eo.ID).Scan(&currentStatus)
			if errRow == nil && currentStatus == "pending" {
				_, _ = tx.Exec("UPDATE key_orders SET status = 'cancelled', updated_at = ? WHERE id = ?", now, eo.ID)
				_, _ = tx.Exec("UPDATE card_stock SET status = 'available', order_id = NULL, updated_at = ? WHERE order_id = ? AND status = 'locked'", now, eo.ID)
				tx.Commit()
				log.Printf("Auto-expired unpaid order %s and released its locked stock keys\n", eo.OutTradeNo)
			} else {
				tx.Rollback()
			}
		}
	} else {
		log.Printf("Background sync: failed to query expired pending orders: %v\n", errExpired)
	}

	// 4. Clean up orchestrator logs
	cleanOrchestratorLogs()
}

func cleanOrchestratorLogs() {
	enabled := getSetting("log_cleanup_open", "off")
	if enabled != "on" {
		return
	}

	daysStr := getSetting("log_cleanup_days", "30")
	days, err := strconv.Atoi(daysStr)
	if err != nil || days <= 0 {
		days = 30
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	res, err := db.Exec("DELETE FROM orchestrator_logs WHERE created_at < ?", cutoff)
	if err != nil {
		log.Printf("Background sync: failed to clean orchestrator logs: %v\n", err)
		return
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected > 0 {
		log.Printf("Background sync: cleaned %d orchestrator logs older than %d days\n", rowsAffected, days)
	}
}

