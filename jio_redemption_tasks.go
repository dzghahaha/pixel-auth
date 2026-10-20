package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const jioTaskTimeout = 10 * time.Minute

func createJioRedemptionTaskTable() {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS jio_redemption_tasks (
		task_id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
		card_secret VARCHAR(255) NOT NULL,
		status VARCHAR(20) NOT NULL DEFAULT 'processing',
		progress INT NOT NULL DEFAULT 0,
		response_json MEDIUMTEXT,
		expires_at DATETIME NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		INDEX idx_jio_task_card (card_secret, created_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		log.Fatalf("Create Jio redemption task table: %v", err)
	}
}

type jioProgressKey struct{}
type jioProgressFunc func(int)

func reportJioProgress(ctx context.Context, percent int) {
	if fn, ok := ctx.Value(jioProgressKey{}).(jioProgressFunc); ok {
		fn(min(99, max(0, percent)))
	}
}

// Estimates are bounded within a stage. The task's database update is monotonic,
// so a rejected number or a supplier fallback never moves the visible bar back.
func beginJioProgressStage(ctx context.Context, from, ceiling int, expected time.Duration) func() {
	if _, ok := ctx.Value(jioProgressKey{}).(jioProgressFunc); !ok {
		return func() {}
	}
	reportJioProgress(ctx, from)
	stageCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stageCtx.Done():
				return
			case <-ticker.C:
				percent := from + int(float64(ceiling-from)*float64(time.Since(start))/float64(expected))
				reportJioProgress(ctx, min(ceiling, percent))
			}
		}
	}()
	return func() { cancel(); <-done }
}

// Capture the existing redemption response so sync and async consumers share
// exactly the same persistence, wallet settlement and maintenance behavior.
type jioTaskResponse struct {
	header http.Header
	bytes.Buffer
	code int
}

func (w *jioTaskResponse) Header() http.Header { return w.header }
func (w *jioTaskResponse) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *jioTaskResponse) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return w.Buffer.Write(p)
}

func startJioRedemptionTask(card string, run func(http.ResponseWriter, context.Context)) (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	id := hex.EncodeToString(token)
	now := time.Now()
	_, err := db.Exec(`INSERT INTO jio_redemption_tasks (task_id,card_secret,status,progress,expires_at,created_at,updated_at) VALUES (?,?,'processing',0,?,?,?)`, id, card, now.Add(jioTaskTimeout+30*time.Second), now, now)
	if err != nil {
		return "", err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), jioTaskTimeout)
		defer cancel()
		var progressMu sync.Mutex
		last := -1
		ctx = context.WithValue(ctx, jioProgressKey{}, jioProgressFunc(func(percent int) {
			progressMu.Lock()
			defer progressMu.Unlock()
			if percent <= last {
				return
			}
			writeCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			if _, err := db.ExecContext(writeCtx, `UPDATE jio_redemption_tasks SET progress = GREATEST(progress, ?), updated_at = NOW() WHERE task_id = ? AND status = 'processing'`, percent, id); err == nil {
				last = percent
			}
		}))
		response := &jioTaskResponse{header: make(http.Header)}
		defer func() {
			if recover() != nil {
				log.Printf("[JioTask] task %s interrupted", id)
				response.Buffer.Reset()
				response.code = 500
				respondJSON(response, 500, map[string]any{"success": false, "message": "兑换任务中断，请联系管理员核查，切勿重复下单"})
			}
			var result struct {
				Success bool `json:"success"`
				Data    struct {
					Status   string `json:"status"`
					OfferURL string `json:"offer_url"`
				} `json:"data"`
			}
			valid := json.Unmarshal(response.Bytes(), &result) == nil
			state, percent := "failed", last
			if valid && result.Success {
				if result.Data.Status == "paused" {
					state = "paused"
				} else if result.Data.OfferURL != "" {
					state, percent = "success", 100
				}
			}
			if !valid {
				response.Buffer.Reset()
				respondJSON(response, 500, map[string]any{"success": false, "message": "兑换结果异常，请联系管理员核查"})
			}
			// Persist even when the provider's deadline has elapsed.
			writeCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if _, err := db.ExecContext(writeCtx, `UPDATE jio_redemption_tasks SET status=?, progress=GREATEST(progress,?), response_json=?, updated_at=NOW() WHERE task_id=? AND status='processing'`, state, max(0, percent), response.String(), id); err != nil {
				log.Printf("[JioTask] failed to save task %s result: %v", id, err)
			}
		}()
		run(response, ctx)
	}()
	return id, nil
}

func respondJioTaskAccepted(w http.ResponseWriter, id string) {
	w.Header().Set("Cache-Control", "no-store")
	respondJSON(w, http.StatusAccepted, map[string]any{"success": true, "data": map[string]any{"status": "processing", "task_id": id, "progress": 0}})
}

func existingJioRedemptionTask(card string) string {
	var id string
	_ = db.QueryRow(`SELECT task_id FROM jio_redemption_tasks WHERE card_secret=? AND status='processing' ORDER BY created_at DESC LIMIT 1`, card).Scan(&id)
	return id
}

func handleJioRedemptionStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		TaskID string `json:"task_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || len(req.TaskID) != 64 {
		respondJSON(w, 400, map[string]any{"success": false, "message": "任务标识无效"})
		return
	}
	if _, err := hex.DecodeString(req.TaskID); err != nil {
		respondJSON(w, 400, map[string]any{"success": false, "message": "任务标识无效"})
		return
	}
	var state, raw string
	var percent int
	var expires time.Time
	err := db.QueryRow(`SELECT status,progress,COALESCE(response_json,''),expires_at FROM jio_redemption_tasks WHERE task_id=?`, req.TaskID).Scan(&state, &percent, &raw, &expires)
	if err != nil {
		code, message := 500, "任务查询失败，请稍后重试"
		if err == sql.ErrNoRows {
			code, message = 404, "任务不存在，请重新查询卡密"
		}
		respondJSON(w, code, map[string]any{"success": false, "message": message})
		return
	}
	if state == "processing" && time.Now().After(expires) {
		state = "failed"
		raw = `{"success":false,"message":"兑换任务已中断或超时，请联系管理员核查，切勿重复下单"}`
		// A restart may have happened after payment; never automatically unlock.
		_, _ = db.Exec(`UPDATE jio_redemption_tasks SET status='failed',response_json=?,updated_at=NOW() WHERE task_id=? AND status='processing'`, raw, req.TaskID)
	}
	data := map[string]any{"task_id": req.TaskID, "status": state, "progress": percent}
	if strings.TrimSpace(raw) != "" {
		data["result"] = json.RawMessage(raw)
	}
	respondJSON(w, 200, map[string]any{"success": true, "data": data})
}
