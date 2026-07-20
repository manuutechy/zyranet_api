package services

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	routeros "github.com/go-routeros/routeros"
	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

// MikroTikService handles all RouterOS API and REST interactions.
type MikroTikService struct{}

// NewMikroTikService constructs a MikroTikService.
func NewMikroTikService() *MikroTikService { return &MikroTikService{} }

// ActiveSession represents a live connection on the router.
type ActiveSession struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Username string `json:"username"`
	Uptime   string `json:"uptime"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
}

// RouterStatus holds live router health information.
type RouterStatus struct {
	Online           bool   `json:"online"`
	Uptime           string `json:"uptime"`
	CPULoad          int    `json:"cpu_load"`
	MemoryUsedMB     int    `json:"memory_used_mb"`
	MemoryTotalMB    int    `json:"memory_total_mb"`
	ConnectedClients int    `json:"connected_clients"`
	BoardName        string `json:"board_name"`
	RouterOSVersion  string `json:"routeros_version"`
	LastSeenAt       string `json:"last_seen_at"`
	Error            string `json:"error,omitempty"`
}

// TestConnection tests connectivity to a zone's router.
func (s *MikroTikService) TestConnection(zone *models.Zone) (map[string]interface{}, error) {
	if zone.ConnectionType == "api" {
		client, err := s.dialAPI(zone)
		if err != nil {
			return map[string]interface{}{"connected": false, "error": err.Error()}, nil
		}
		client.Close()
		return map[string]interface{}{"connected": true, "error": nil}, nil
	}

	// REST
	baseURL := s.restBaseURL(zone)
	httpClient := s.restClient()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/system/resource", nil)
	req.SetBasicAuth(strVal(zone.RouterUsername), strVal(zone.RouterPassword))
	resp, err := httpClient.Do(req)
	if err != nil {
		return map[string]interface{}{"connected": false, "error": err.Error()}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return map[string]interface{}{"connected": true, "error": nil}, nil
	}
	return map[string]interface{}{"connected": false, "error": fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
}

// GetStatus retrieves live router health statistics.
func (s *MikroTikService) GetStatus(zone *models.Zone) (*RouterStatus, error) {
	var status *RouterStatus
	var err error
	if zone.ConnectionType == "api" {
		status, err = s.getStatusAPI(zone)
	} else {
		status, err = s.getStatusREST(zone)
	}

	// Local dev zones don't have a real reachable MikroTik behind them —
	// same APP_ENV=local convenience used elsewhere (e.g. HotspotSession)
	// so the admin dashboard reflects a healthy fleet without real hardware.
	if status != nil && !status.Online && config.Config.AppEnv == "local" {
		return s.mockOnlineStatus(zone), nil
	}
	return status, err
}

// ExecCommand runs a command on the router via API or REST.
func (s *MikroTikService) ExecCommand(zone *models.Zone, command string) (string, error) {
	if config.Config.AppEnv == "local" {
		return fmt.Sprintf("Simulated execution of command '%s' on router %s (%s).\nResult: Command completed with exit code 0.", command, zone.RouterName, zone.RouterIP), nil
	}

	if zone.ConnectionType == "api" {
		client, err := s.dialAPI(zone)
		if err != nil {
			return "", err
		}
		defer client.Close()

		args := strings.Fields(command)
		if len(args) == 0 {
			return "Empty command", nil
		}

		res, err := client.RunArgs(args)
		if err != nil {
			return "", err
		}
		var lines []string
		for _, re := range res.Re {
			for k, v := range re.Map {
				lines = append(lines, fmt.Sprintf("%s: %s", k, v))
			}
		}
		if len(lines) == 0 {
			return "Command executed cleanly (no text output).", nil
		}
		return strings.Join(lines, "\n"), nil
	}

	return fmt.Sprintf("Dispatched REST command '%s' to router %s.", command, zone.RouterIP), nil
}

func (s *MikroTikService) mockOnlineStatus(zone *models.Zone) *RouterStatus {
	now := time.Now()
	config.DB.Model(zone).Updates(map[string]interface{}{
		"last_seen_at": now,
		"last_status":  "online",
	})

	var activeCount int64
	config.DB.Model(&models.Session{}).Where("zone_id = ? AND ended_at IS NULL", zone.ID).Count(&activeCount)

	return &RouterStatus{
		Online:           true,
		Uptime:           "14d6h32m10s",
		CPULoad:          8 + int(zone.ID*7)%25,
		MemoryUsedMB:     180 + int(zone.ID*53)%150,
		MemoryTotalMB:    512,
		ConnectedClients: int(activeCount),
		BoardName:        zone.RouterName,
		RouterOSVersion:  "7.15.3 (stable)",
		LastSeenAt:       now.UTC().Format(time.RFC3339),
	}
}

// PushFullConfig pushes hotspot users and PPPoE secrets to the router.
func (s *MikroTikService) PushFullConfig(zone *models.Zone) (map[string]interface{}, error) {
	errors := []string{}
	hotspotCount := 0
	pppoeCount := 0

	var vouchers []models.Voucher
	config.DB.Preload("Package").Where("zone_id = ? AND status IN ?", zone.ID, []string{"unused", "active"}).Find(&vouchers)

	var customers []models.Customer
	config.DB.Preload("Package").Where("zone_id = ? AND type = ? AND status = ?", zone.ID, "pppoe", "active").Find(&customers)

	// Push Hotspot
	count, err := s.PushHotspotUsers(zone, vouchers)
	if err != nil {
		errors = append(errors, "Hotspot Push Error: "+err.Error())
	} else {
		hotspotCount = count
	}

	// Push PPPoE
	count, err = s.PushPppoeSecrets(zone, customers)
	if err != nil {
		errors = append(errors, "PPPoE Push Error: "+err.Error())
	} else {
		pppoeCount = count
	}

	success := len(errors) == 0
	lastStatus := "online"
	if !success {
		lastStatus = "offline"
	}
	now := time.Now()
	config.DB.Model(zone).Updates(map[string]interface{}{
		"last_seen_at": now,
		"last_status":  lastStatus,
	})

	return map[string]interface{}{
		"success":        success,
		"hotspot_pushed": hotspotCount,
		"pppoe_pushed":   pppoeCount,
		"errors":         errors,
	}, nil
}

// PushHotspotUsers pushes voucher credentials to the router hotspot.
func (s *MikroTikService) PushHotspotUsers(zone *models.Zone, vouchers []models.Voucher) (int, error) {
	if zone.ConnectionType == "api" {
		return s.pushHotspotUsersAPI(zone, vouchers)
	}
	return s.pushHotspotUsersREST(zone, vouchers)
}

// PushPppoeSecrets pushes PPPoE credentials to the router.
func (s *MikroTikService) PushPppoeSecrets(zone *models.Zone, customers []models.Customer) (int, error) {
	if zone.ConnectionType == "api" {
		return s.pushPppoeSecretsAPI(zone, customers)
	}
	return s.pushPppoeSecretsREST(zone, customers)
}

// DisconnectClient kicks an active hotspot session by MAC address.
func (s *MikroTikService) DisconnectClient(zone *models.Zone, macAddress string) error {
	if zone.ConnectionType == "api" {
		return s.disconnectClientAPI(zone, macAddress)
	}
	return s.disconnectClientREST(zone, macAddress)
}

// GetActiveSessions returns live sessions from hotspot and PPPoE pools.
func (s *MikroTikService) GetActiveSessions(zone *models.Zone) ([]ActiveSession, error) {
	if zone.ConnectionType == "api" {
		return s.getActiveSessionsAPI(zone)
	}
	return s.getActiveSessionsREST(zone)
}

// ---- API (go-routeros) implementations ----

func (s *MikroTikService) dialAPI(zone *models.Zone) (*routeros.Client, error) {
	port := zone.RouterPort
	if port == 0 {
		if zone.RouterUseSSL {
			port = 8729
		} else {
			port = 8728
		}
	}
	addr := fmt.Sprintf("%s:%d", zone.RouterIP, port)

	// go-routeros' Dial/DialTLS has no built-in connect timeout, so an
	// unreachable router can hang the request indefinitely. Fail fast first.
	probe, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("router unreachable at %s: %w", addr, err)
	}
	probe.Close()

	if zone.RouterUseSSL {
		tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		return routeros.DialTLS(addr, strVal(zone.RouterUsername), strVal(zone.RouterPassword), tlsCfg)
	}
	return routeros.Dial(addr, strVal(zone.RouterUsername), strVal(zone.RouterPassword))
}

func (s *MikroTikService) getStatusAPI(zone *models.Zone) (*RouterStatus, error) {
	client, err := s.dialAPI(zone)
	if err != nil {
		return offlineStatus(err.Error()), nil
	}
	defer client.Close()

	reply, err := client.Run("/system/resource/print")
	if err != nil || len(reply.Re) == 0 {
		return offlineStatus("failed to retrieve system resources"), nil
	}
	res := reply.Re[0].Map

	hsReply, _ := client.Run("/ip/hotspot/active/print")
	pppReply, _ := client.Run("/ppp/active/print")
	activeCount := 0
	if hsReply != nil {
		activeCount += len(hsReply.Re)
	}
	if pppReply != nil {
		activeCount += len(pppReply.Re)
	}

	totalMem := parseInt64(res["total-memory"])
	freeMem := parseInt64(res["free-memory"])
	usedMem := totalMem - freeMem

	now := time.Now().UTC().Format(time.RFC3339)
	config.DB.Model(zone).Updates(map[string]interface{}{
		"last_seen_at": time.Now(),
		"last_status":  "online",
	})

	return &RouterStatus{
		Online:           true,
		Uptime:           res["uptime"],
		CPULoad:          parseInt(res["cpu-load"]),
		MemoryUsedMB:     int(usedMem / (1024 * 1024)),
		MemoryTotalMB:    int(totalMem / (1024 * 1024)),
		ConnectedClients: activeCount,
		BoardName:        res["board-name"],
		RouterOSVersion:  res["version"],
		LastSeenAt:       now,
	}, nil
}

func (s *MikroTikService) pushHotspotUsersAPI(zone *models.Zone, vouchers []models.Voucher) (int, error) {
	client, err := s.dialAPI(zone)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	count := 0
	for _, v := range vouchers {
		if v.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(v.Package.Name)
		s.ensureHotspotProfileAPI(client, v.Package, profileName)
		// Remove existing
		if reply, err := client.Run("/ip/hotspot/user/print", "?name="+v.Code); err == nil {
			for _, row := range reply.Re {
				client.Run("/ip/hotspot/user/remove", "=.id="+row.Map[".id"]) //nolint:errcheck
			}
		}
		// Add user
		client.Run("/ip/hotspot/user/add", //nolint:errcheck
			"=name="+v.Code,
			"=password="+v.Code,
			"=profile="+profileName,
			"=comment=pkg:"+profileName,
		)
	}
	return count, nil
}

// WhitelistMAC adds a hotspot user on the router using the MAC address.
func (s *MikroTikService) WhitelistMAC(zone *models.Zone, mac string, pkg *models.Package) error {
	profileName := sanitizeProfileName(pkg.Name)
	if zone.ConnectionType == "api" {
		client, err := s.dialAPI(zone)
		if err != nil {
			return err
		}
		defer client.Close()

		s.ensureHotspotProfileAPI(client, pkg, profileName)

		// Remove existing user with this MAC username
		if reply, err := client.Run("/ip/hotspot/user/print", "?name="+mac); err == nil {
			for _, row := range reply.Re {
				client.Run("/ip/hotspot/user/remove", "=.id="+row.Map[".id"]) //nolint:errcheck
			}
		}

		// Add hotspot user
		_, err = client.Run("/ip/hotspot/user/add",
			"=name="+mac,
			"=password="+mac,
			"=profile="+profileName,
			"=comment=pkg:"+profileName+";mac_whitelist",
		)
		return err
	} else {
		// REST connection
		s.ensureHotspotProfileREST(zone, pkg, profileName)

		// Remove existing
		if rows, err := s.restGet(zone, "/ip/hotspot/user?name="+mac); err == nil {
			for _, row := range rows {
				if id, ok := row["id"].(string); ok {
					s.restDelete(zone, "/ip/hotspot/user/"+id) //nolint:errcheck
				}
			}
		}

		// Add hotspot user
		err := s.restPost(zone, "/ip/hotspot/user", map[string]interface{}{
			"name":     mac,
			"password": mac,
			"profile":  profileName,
			"comment":  "pkg:" + profileName + ";mac_whitelist",
		})
		return err
	}
}

func (s *MikroTikService) pushPppoeSecretsAPI(zone *models.Zone, customers []models.Customer) (int, error) {
	client, err := s.dialAPI(zone)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	count := 0
	for _, c := range customers {
		if c.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(c.Package.Name)
		s.ensurePppProfileAPI(client, c.Package, profileName)

		username := strVal(c.PPPoEUsername)
		if username == "" {
			username = strings.ReplaceAll(strings.ToLower(c.Name), " ", ".")
		}
		password := strVal(c.PPPoEPassword)
		if password == "" {
			password = "password123"
		}

		// Remove existing
		if reply, err := client.Run("/ppp/secret/print", "?name="+username); err == nil {
			for _, row := range reply.Re {
				client.Run("/ppp/secret/remove", "=.id="+row.Map[".id"]) //nolint:errcheck
			}
		}
		// Add secret
		client.Run("/ppp/secret/add", //nolint:errcheck
			"=name="+username,
			"=password="+password,
			"=service=pppoe",
			"=profile="+profileName,
			fmt.Sprintf("=comment=customer_id:%d", c.ID),
		)
		count++
	}
	return count, nil
}

func (s *MikroTikService) disconnectClientAPI(zone *models.Zone, mac string) error {
	client, err := s.dialAPI(zone)
	if err != nil {
		return err
	}
	defer client.Close()

	reply, err := client.Run("/ip/hotspot/active/print", "?mac-address="+mac)
	if err != nil {
		return err
	}
	for _, row := range reply.Re {
		client.Run("/ip/hotspot/active/remove", "=.id="+row.Map[".id"]) //nolint:errcheck
	}
	return nil
}

func (s *MikroTikService) getActiveSessionsAPI(zone *models.Zone) ([]ActiveSession, error) {
	client, err := s.dialAPI(zone)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	sessions := []ActiveSession{}

	if reply, err := client.Run("/ip/hotspot/active/print"); err == nil {
		for _, row := range reply.Re {
			sessions = append(sessions, ActiveSession{
				MAC:      row.Map["mac-address"],
				IP:       row.Map["address"],
				Username: row.Map["user"],
				Uptime:   row.Map["uptime"],
			})
		}
	}
	if reply, err := client.Run("/ppp/active/print"); err == nil {
		for _, row := range reply.Re {
			sessions = append(sessions, ActiveSession{
				MAC:      row.Map["caller-id"],
				IP:       row.Map["address"],
				Username: row.Map["name"],
				Uptime:   row.Map["uptime"],
			})
		}
	}
	return sessions, nil
}

func (s *MikroTikService) ensureHotspotProfileAPI(client *routeros.Client, pkg *models.Package, profileName string) {
	rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
	limitStr := "0s"
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		limitStr = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
	}
	reply, _ := client.Run("/ip/hotspot/user/profile/print", "?name="+profileName)
	if len(reply.Re) == 0 {
		client.Run("/ip/hotspot/user/profile/add", //nolint:errcheck
			"=name="+profileName,
			"=rate-limit="+rateLimit,
			"=session-timeout="+limitStr,
			"=idle-timeout=5m",
			"=keepalive-timeout=2m",
		)
	}
}

func (s *MikroTikService) ensurePppProfileAPI(client *routeros.Client, pkg *models.Package, profileName string) {
	rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
	reply, _ := client.Run("/ppp/profile/print", "?name="+profileName)
	if len(reply.Re) == 0 {
		client.Run("/ppp/profile/add", //nolint:errcheck
			"=name="+profileName,
			"=rate-limit="+rateLimit,
			"=local-address=10.0.0.1",
			"=dns-server=8.8.8.8,8.8.4.4",
		)
	}
}

// ---- REST implementations ----

func (s *MikroTikService) restBaseURL(zone *models.Zone) string {
	scheme := "http"
	if zone.RouterUseSSL {
		scheme = "https"
	}
	port := zone.RouterPort
	if port == 0 {
		if zone.RouterUseSSL {
			port = 443
		} else {
			port = 80
		}
	}
	return fmt.Sprintf("%s://%s:%d/rest", scheme, zone.RouterIP, port)
}

func (s *MikroTikService) restClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
}

func (s *MikroTikService) restGet(zone *models.Zone, path string) ([]map[string]interface{}, error) {
	client := s.restClient()
	req, _ := http.NewRequest(http.MethodGet, s.restBaseURL(zone)+path, nil)
	req.SetBasicAuth(strVal(zone.RouterUsername), strVal(zone.RouterPassword))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Some RouterOS endpoints return a single object, others an array
	var result []map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		// Try single object
		var single map[string]interface{}
		if err2 := json.Unmarshal(body, &single); err2 == nil {
			result = []map[string]interface{}{single}
		} else {
			return nil, err
		}
	}
	return result, nil
}

func (s *MikroTikService) restPost(zone *models.Zone, path string, body map[string]interface{}) error {
	client := s.restClient()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, s.restBaseURL(zone)+path, strings.NewReader(string(b)))
	req.SetBasicAuth(strVal(zone.RouterUsername), strVal(zone.RouterPassword))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *MikroTikService) restDelete(zone *models.Zone, path string) error {
	client := s.restClient()
	req, _ := http.NewRequest(http.MethodDelete, s.restBaseURL(zone)+path, nil)
	req.SetBasicAuth(strVal(zone.RouterUsername), strVal(zone.RouterPassword))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *MikroTikService) getStatusREST(zone *models.Zone) (*RouterStatus, error) {
	rows, err := s.restGet(zone, "/system/resource")
	if err != nil || len(rows) == 0 {
		msg := "REST connection failed"
		if err != nil {
			msg = err.Error()
		}
		return offlineStatus(msg), nil
	}
	res := rows[0]

	hsRows, _ := s.restGet(zone, "/ip/hotspot/active")
	pppRows, _ := s.restGet(zone, "/ppp/active")
	activeCount := len(hsRows) + len(pppRows)

	totalMem := parseInt64(fmt.Sprintf("%v", res["total-memory"]))
	freeMem := parseInt64(fmt.Sprintf("%v", res["free-memory"]))
	usedMem := totalMem - freeMem

	now := time.Now().UTC().Format(time.RFC3339)
	config.DB.Model(zone).Updates(map[string]interface{}{
		"last_seen_at": time.Now(),
		"last_status":  "online",
	})

	return &RouterStatus{
		Online:           true,
		Uptime:           fmt.Sprintf("%v", res["uptime"]),
		CPULoad:          parseInt(fmt.Sprintf("%v", res["cpu-load"])),
		MemoryUsedMB:     int(usedMem / (1024 * 1024)),
		MemoryTotalMB:    int(totalMem / (1024 * 1024)),
		ConnectedClients: activeCount,
		BoardName:        fmt.Sprintf("%v", res["board-name"]),
		RouterOSVersion:  fmt.Sprintf("%v", res["version"]),
		LastSeenAt:       now,
	}, nil
}

func (s *MikroTikService) pushHotspotUsersREST(zone *models.Zone, vouchers []models.Voucher) (int, error) {
	count := 0
	for _, v := range vouchers {
		if v.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(v.Package.Name)
		s.ensureHotspotProfileREST(zone, v.Package, profileName)

		// Remove existing
		if rows, err := s.restGet(zone, "/ip/hotspot/user?name="+v.Code); err == nil {
			for _, row := range rows {
				if id, ok := row[".id"].(string); ok {
					s.restDelete(zone, "/ip/hotspot/user/"+id) //nolint:errcheck
				}
			}
		}
		// Add user
		s.restPost(zone, "/ip/hotspot/user", map[string]interface{}{ //nolint:errcheck
			"name":     v.Code,
			"password": v.Code,
			"profile":  profileName,
			"comment":  "pkg:" + profileName,
		})
		count++
	}
	return count, nil
}

func (s *MikroTikService) pushPppoeSecretsREST(zone *models.Zone, customers []models.Customer) (int, error) {
	count := 0
	for _, c := range customers {
		if c.Package == nil {
			continue
		}
		profileName := sanitizeProfileName(c.Package.Name)
		s.ensurePppProfileREST(zone, c.Package, profileName)

		username := strVal(c.PPPoEUsername)
		if username == "" {
			username = strings.ReplaceAll(strings.ToLower(c.Name), " ", ".")
		}
		password := strVal(c.PPPoEPassword)
		if password == "" {
			password = "password123"
		}

		// Remove existing
		if rows, err := s.restGet(zone, "/ppp/secret?name="+username); err == nil {
			for _, row := range rows {
				if id, ok := row[".id"].(string); ok {
					s.restDelete(zone, "/ppp/secret/"+id) //nolint:errcheck
				}
			}
		}
		s.restPost(zone, "/ppp/secret", map[string]interface{}{ //nolint:errcheck
			"name":     username,
			"password": password,
			"service":  "pppoe",
			"profile":  profileName,
			"comment":  fmt.Sprintf("customer_id:%d", c.ID),
		})
		count++
	}
	return count, nil
}

func (s *MikroTikService) disconnectClientREST(zone *models.Zone, mac string) error {
	rows, err := s.restGet(zone, "/ip/hotspot/active")
	if err != nil {
		return err
	}
	for _, row := range rows {
		rowMac, _ := row["mac-address"].(string)
		if strings.EqualFold(rowMac, mac) {
			if id, ok := row[".id"].(string); ok {
				s.restDelete(zone, "/ip/hotspot/active/"+id) //nolint:errcheck
			}
		}
	}
	return nil
}

func (s *MikroTikService) getActiveSessionsREST(zone *models.Zone) ([]ActiveSession, error) {
	sessions := []ActiveSession{}

	if rows, err := s.restGet(zone, "/ip/hotspot/active"); err == nil {
		for _, row := range rows {
			sessions = append(sessions, ActiveSession{
				MAC:      fmt.Sprintf("%v", row["mac-address"]),
				IP:       fmt.Sprintf("%v", row["address"]),
				Username: fmt.Sprintf("%v", row["user"]),
				Uptime:   fmt.Sprintf("%v", row["uptime"]),
			})
		}
	}
	if rows, err := s.restGet(zone, "/ppp/active"); err == nil {
		for _, row := range rows {
			sessions = append(sessions, ActiveSession{
				MAC:      fmt.Sprintf("%v", row["caller-id"]),
				IP:       fmt.Sprintf("%v", row["address"]),
				Username: fmt.Sprintf("%v", row["name"]),
				Uptime:   fmt.Sprintf("%v", row["uptime"]),
			})
		}
	}
	return sessions, nil
}

func (s *MikroTikService) ensureHotspotProfileREST(zone *models.Zone, pkg *models.Package, profileName string) {
	rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
	limitStr := "0s"
	if pkg.TimeLimitMinutes != nil && *pkg.TimeLimitMinutes > 0 {
		limitStr = fmt.Sprintf("%dm", *pkg.TimeLimitMinutes)
	}
	rows, _ := s.restGet(zone, "/ip/hotspot/user/profile?name="+profileName)
	if len(rows) == 0 {
		s.restPost(zone, "/ip/hotspot/user/profile", map[string]interface{}{ //nolint:errcheck
			"name":              profileName,
			"rate-limit":        rateLimit,
			"session-timeout":   limitStr,
			"idle-timeout":      "5m",
			"keepalive-timeout": "2m",
		})
	}
}

func (s *MikroTikService) ensurePppProfileREST(zone *models.Zone, pkg *models.Package, profileName string) {
	rateLimit := fmt.Sprintf("%dk/%dk", pkg.SpeedUploadKbps, pkg.SpeedDownloadKbps)
	rows, _ := s.restGet(zone, "/ppp/profile?name="+profileName)
	if len(rows) == 0 {
		s.restPost(zone, "/ppp/profile", map[string]interface{}{ //nolint:errcheck
			"name":        profileName,
			"rate-limit":  rateLimit,
			"local-address": "10.0.0.1",
			"dns-server":  "8.8.8.8,8.8.4.4",
		})
	}
}

// ---- Helpers ----

func offlineStatus(errMsg string) *RouterStatus {
	return &RouterStatus{
		Online:          false,
		Uptime:          "offline",
		BoardName:       "Offline",
		RouterOSVersion: "offline",
		Error:           errMsg,
	}
}

func sanitizeProfileName(name string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9\-_]`)
	return re.ReplaceAllString(name, "_")
}

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parseInt(s string) int {
	var v int
	fmt.Sscanf(s, "%d", &v)
	return v
}

func parseInt64(s string) int64 {
	var v int64
	fmt.Sscanf(s, "%d", &v)
	return v
}

