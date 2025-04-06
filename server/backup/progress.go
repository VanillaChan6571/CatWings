package backup

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/pterodactyl/wings/events"
)

// ZipProgressMonitor tracks the progress of a ZIP file creation
// by monitoring its size over time
type ZipProgressMonitor struct {
	filePath       string
	serverUUID     string
	events         *events.Bus
	interval       time.Duration
	maxUnchanged   int
	stopChan       chan struct{}
	logger         *log.Entry
	mu             sync.Mutex
	running        bool
	lastPercentage int
}

// NewZipProgressMonitor creates a new monitor for tracking ZIP progress
func NewZipProgressMonitor(filePath string, serverUUID string, events *events.Bus) *ZipProgressMonitor {
	return &ZipProgressMonitor{
		filePath:       filePath,
		serverUUID:     serverUUID,
		events:         events,
		interval:       5 * time.Second,
		maxUnchanged:   10, // 50 seconds without changes = completion
		stopChan:       make(chan struct{}),
		logger:         log.WithField("component", "zip-monitor").WithField("file", filePath).WithField("server", serverUUID),
		lastPercentage: 0,
	}
}

// Start begins monitoring the file size changes
func (m *ZipProgressMonitor) Start(ctx context.Context) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()

		var lastSize int64
		var unchangedCount int
		var lastReportTime time.Time

		m.logger.Debug("starting zip progress monitoring")

		for {
			select {
			case <-ticker.C:
				stat, err := os.Stat(m.filePath)
				if err != nil {
					if !os.IsNotExist(err) {
						m.logger.WithError(err).Warn("failed to stat zip file")
					}
					continue
				}

				currentSize := stat.Size()

				// If we've seen this size before, increment the counter
				if currentSize == lastSize {
					unchangedCount++
					m.logger.WithField("unchanged_count", unchangedCount).
						WithField("max", m.maxUnchanged).
						Debug("zip file size unchanged")

					if unchangedCount >= m.maxUnchanged {
						m.logger.Info("zip file size stabilized, considering complete")
						m.publishProgress(100) // Mark as 100% complete
						m.Stop()
						return
					}
				} else {
					unchangedCount = 0
					lastSize = currentSize
					m.logger.WithField("size", currentSize).Debug("zip file size changed")
				}

				// Publish progress updates at reasonable intervals
				if time.Since(lastReportTime) > time.Second*10 {
					// Calculate a pseudo-progress based on file size changes
					// This is not accurate but gives users some feedback
					progress := m.calculateProgress(unchangedCount, currentSize)
					if progress > m.lastPercentage {
						m.publishProgress(progress)
						m.lastPercentage = progress
						lastReportTime = time.Now()
					}
				}

			case <-m.stopChan:
				m.logger.Debug("zip monitoring stopped")
				return

			case <-ctx.Done():
				m.logger.Debug("zip monitoring canceled by context")
				return
			}
		}
	}()
}

// calculateProgress estimates the progress percentage based on file activity
// This is a rough estimate since we don't know the final size
func (m *ZipProgressMonitor) calculateProgress(unchangedCount int, size int64) int {
	// If file size hasn't changed in a while, estimate higher progress
	if unchangedCount > 0 {
		// Scale from 50% to 99% based on unchanged count
		progress := 50 + (unchangedCount * 49 / m.maxUnchanged)
		if progress > 99 {
			progress = 99
		}
		return progress
	}

	// For active compression, use file size as a rough indicator
	// Assume most small backups will be < 100MB, medium < 1GB, large < 10GB
	if size < 1024*1024*100 { // 100MB
		return int((float64(size) / (1024 * 1024 * 100)) * 50)
	} else if size < 1024*1024*1024 { // 1GB
		return 50 + int((float64(size-1024*1024*100)/(1024*1024*900))*30)
	} else {
		return 80 + int(float64(size-1024*1024*1024)/(1024*1024*1024*9)*15)
	}
}

// publishProgress sends progress updates to the event bus
func (m *ZipProgressMonitor) publishProgress(percentage int) {
	if m.events != nil {
		m.events.Publish(BackupCompletingEvent, map[string]interface{}{
			"backup_id":  m.serverUUID,
			"percentage": percentage,
		})
	}
}

// Stop halts the monitoring process
func (m *ZipProgressMonitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running {
		return
	}

	close(m.stopChan)
	m.running = false
}
