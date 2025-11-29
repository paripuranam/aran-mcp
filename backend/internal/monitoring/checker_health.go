package monitoring

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/radhi1991/aran-mcp-sentinel/internal/database"
	"go.uber.org/zap"
)

// HealthChecker handles MCP server health monitoring
type HealthChecker struct {
	repo   *database.Repository
	logger *zap.Logger
	client *http.Client
}

// HealthStatus represents the health status of an MCP server
type HealthStatus struct {
	ServerID      string    `json:"server_id"`
	Status        string    `json:"status"`        // online, offline, error, unknown
	ResponseTime  int64     `json:"response_time"` // milliseconds
	LastChecked   time.Time `json:"last_checked"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	Uptime        string    `json:"uptime,omitempty"`
	MemoryUsage   string    `json:"memory_usage,omitempty"`
	Version       string    `json:"version,omitempty"`
	Capabilities  []string  `json:"capabilities,omitempty"`
}

// NewHealthChecker creates a new health checker instance
func NewHealthChecker(repo *database.Repository, logger *zap.Logger) *HealthChecker {
	return &HealthChecker{
		repo:   repo,
		logger: logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// CheckServerHealth performs a health check on a specific MCP server
func (hc *HealthChecker) CheckServerHealth(ctx context.Context, serverID string) (*HealthStatus, error) {
	// Get server details from database
	server, err := hc.repo.GetMCPServer(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("failed to get server: %w", err)
	}

	status := &HealthStatus{
		ServerID:     serverID,
		LastChecked:  time.Now(),
		Status:       "unknown",
		ResponseTime: 0,
	}

	// Perform health check based on server type
	switch server.Type {
	case "http", "https":
		err = hc.checkHTTPServer(ctx, server.URL, status)
	case "filesystem":
		err = hc.checkFilesystemServer(ctx, server.URL, status)
	default:
		err = hc.checkGenericServer(ctx, server.URL, status)
	}

	if err != nil {
		status.Status = "error"
		status.ErrorMessage = err.Error()
		hc.logger.Error("Health check failed", 
			zap.String("server_id", serverID),
			zap.String("url", server.URL),
			zap.Error(err))
	} else {
		status.Status = "online"
		hc.logger.Info("Health check successful",
			zap.String("server_id", serverID),
			zap.String("url", server.URL),
			zap.Int64("response_time", status.ResponseTime))
	}

	// Update server status in database
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		hc.logger.Error("Invalid server ID", zap.String("server_id", serverID), zap.Error(err))
	} else {
		responseTimeMs := int(status.ResponseTime)
		var errorMsg *string
		if status.ErrorMessage != "" {
			errorMsg = &status.ErrorMessage
		}
		
		updateErr := hc.repo.UpdateMCPServerStatus(ctx, serverUUID, status.Status, &responseTimeMs, errorMsg)
		if updateErr != nil {
			hc.logger.Error("Failed to update server status", 
				zap.String("server_id", serverID),
				zap.Error(updateErr))
		}
	}

	// Log status change if needed
	if server.Status != status.Status {
		hc.logStatusChange(ctx, serverID, server.Status, status.Status, status.ErrorMessage)
	}

	return status, nil
}

// checkHTTPServer performs health check for HTTP/HTTPS servers
func (hc *HealthChecker) checkHTTPServer(ctx context.Context, url string, status *HealthStatus) error {
	start := time.Now()
	
	req, err := http.NewRequestWithContext(ctx, "GET", url+"/health", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := hc.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	status.ResponseTime = time.Since(start).Milliseconds()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	// Try to parse server info from response headers
	if version := resp.Header.Get("X-Server-Version"); version != "" {
		status.Version = version
	}
	if uptime := resp.Header.Get("X-Server-Uptime"); uptime != "" {
		status.Uptime = uptime
	}
	if memory := resp.Header.Get("X-Server-Memory"); memory != "" {
		status.MemoryUsage = memory
	}

	return nil
}

// checkFilesystemServer performs health check for filesystem-based MCP servers
func (hc *HealthChecker) checkFilesystemServer(ctx context.Context, url string, status *HealthStatus) error {
	// For filesystem servers, we'll check if the process is running
	// This is a simplified check - in production, you might want to use process monitoring
	start := time.Now()
	
	req, err := http.NewRequestWithContext(ctx, "GET", url+"/info", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := hc.client.Do(req)
	if err != nil {
		return fmt.Errorf("filesystem server not responding: %w", err)
	}
	defer resp.Body.Close()

	status.ResponseTime = time.Since(start).Milliseconds()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("filesystem server returned status %d", resp.StatusCode)
	}

	// Set default capabilities for filesystem servers
	status.Capabilities = []string{"read_file", "write_file", "list_directory", "get_server_info"}
	status.Version = "1.0.0"

	return nil
}

// checkGenericServer performs a generic health check
func (hc *HealthChecker) checkGenericServer(ctx context.Context, url string, status *HealthStatus) error {
	start := time.Now()
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := hc.client.Do(req)
	if err != nil {
		return fmt.Errorf("server not responding: %w", err)
	}
	defer resp.Body.Close()

	status.ResponseTime = time.Since(start).Milliseconds()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	return nil
}

// logStatusChange creates an alert when server status changes
func (hc *HealthChecker) logStatusChange(ctx context.Context, serverID, oldStatus, newStatus, errorMessage string) {
	severity := "info"
	message := fmt.Sprintf("Server status changed from %s to %s", oldStatus, newStatus)

	if newStatus == "error" || newStatus == "offline" {
		severity = "warning"
		message = fmt.Sprintf("Server went %s: %s", newStatus, errorMessage)
	} else if newStatus == "online" && oldStatus != "online" {
		severity = "info"
		message = fmt.Sprintf("Server came back online")
	}

	// Create alert
	serverUUID, err := uuid.Parse(serverID)
	if err != nil {
		hc.logger.Error("Invalid server ID for alert", zap.String("server_id", serverID), zap.Error(err))
		return
	}

	alert := &database.Alert{
		ServerID: &serverUUID,
		Type:     "status_change",
		Severity: severity,
		Title:    "Server Status Change",
		Message:  message,
	}

	if err := hc.repo.CreateAlert(ctx, alert); err != nil {
		hc.logger.Error("Failed to create status change alert",
			zap.String("server_id", serverID),
			zap.Error(err))
	}
}

// CheckAllServers performs health checks on all active MCP servers
func (hc *HealthChecker) CheckAllServers(ctx context.Context) error {
	servers, err := hc.repo.ListActiveMCPServers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get active servers: %w", err)
	}

	hc.logger.Info("Starting health check for all servers",
		zap.Int("server_count", len(servers)))

	for _, server := range servers {
		_, err := hc.CheckServerHealth(ctx, server.ID.String())
		if err != nil {
			hc.logger.Error("Health check failed for server",
				zap.String("server_id", server.ID.String()),
				zap.String("server_name", server.Name),
				zap.Error(err))
		}
	}

	return nil
}

// StartPeriodicHealthChecks starts a background routine for periodic health checks
func (hc *HealthChecker) StartPeriodicHealthChecks(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	hc.logger.Info("Starting periodic health checks",
		zap.Duration("interval", interval))

	for {
		select {
		case <-ctx.Done():
			hc.logger.Info("Stopping periodic health checks")
			return
		case <-ticker.C:
			if err := hc.CheckAllServers(ctx); err != nil {
				hc.logger.Error("Periodic health check failed", zap.Error(err))
			}
		}
	}
}
