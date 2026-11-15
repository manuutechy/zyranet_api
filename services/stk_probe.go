package services

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"github.com/zyranet/zyranet-api/utils"
)

// TestPrompt is the admin "Test STK" button: one prompt to the given phone
// through whichever provider the zone's ISP actually uses (cyzora | daraja),
// so the test proves the path real customers take. No Payment row is made,
// so a test never shows up as takings. A paid Cyzora test charge reaches the
// webhook as an unknown charge: it is logged and acknowledged, nothing else.
func (s *MpesaService) TestPrompt(zoneID uint, phone string, amount float64) (*STKStart, error) {
	provider, _ := PaymentRouting(zoneID)
	if provider != ProviderCyzora {
		resp, err := s.InitiateSTKPush(zoneID, phone, amount, "SA-TEST", "Zyra Net Daraja Test Payment")
		if err != nil {
			return nil, err
		}
		return &STKStart{Provider: ProviderDaraja, Reference: resp.CheckoutRequestID, Message: resp.ResponseDescription, IsMock: resp.IsMock}, nil
	}

	p := utils.FormatPhone(phone)
	if len(p) != 12 || (!strings.HasPrefix(p, "2547") && !strings.HasPrefix(p, "2541")) {
		return nil, fmt.Errorf("invalid phone number: must be 12 digits (e.g. 2547XXXXXXXX or 2541XXXXXXXX)")
	}
	kes := math.Floor(amount)
	if kes < 1 {
		return nil, fmt.Errorf("amount must be at least 1 KES")
	}
	// Local/test servers with M-Pesa mocked never call Cyzora.
	if strings.EqualFold(config.Config.MpesaEnv, "mock") && (config.Config.AppEnv == "local" || config.Config.AppEnv == "test") {
		return &STKStart{Provider: ProviderCyzora, Reference: "cyz_mock_" + randomToken(8), Message: "Mock Cyzora prompt (nothing sent).", IsMock: true}, nil
	}
	if !CyzoraConfigured() {
		return nil, fmt.Errorf("Cyzora is this ISP's payment provider but no Cyzora secret key is saved")
	}
	var zone models.Zone
	config.DB.Select("organization_id").First(&zone, zoneID)
	meta := map[string]string{
		"zyra_test": "1",
		"org_id":    strconv.FormatUint(uint64(zone.OrganizationID), 10),
		"zone_id":   strconv.FormatUint(uint64(zoneID), 10),
	}
	charge, accepted, err := s.cyzoraClient().CreateCharge("zyranet-stktest-"+randomToken(12), int64(kes)*100, p, "ZYRTEST", meta)
	if err != nil {
		return nil, err
	}
	out := &STKStart{Provider: ProviderCyzora, Message: "Check the phone and enter the M-Pesa PIN."}
	if charge != nil {
		out.Reference = charge.ID
	}
	if accepted {
		out.Message = "Cyzora accepted the prompt; the outcome isn't known yet."
	}
	if CyzoraKeyMode(CyzoraSecretKey()) == "test" {
		out.IsMock = true
		out.Message = "Cyzora test key: the prompt is simulated, no money moves."
	}
	return out, nil
}
