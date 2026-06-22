package services

import (
	"testing"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

func newTestMpesaService() *MpesaService {
	sms := NewSmsService()
	voucher := NewVoucherService(sms)
	mikrotik := NewMikroTikService()
	return NewMpesaService(sms, voucher, mikrotik)
}

// seedZone creates a zone + package and returns their IDs.
func seedZone(t *testing.T) (zoneID, packageID uint) {
	t.Helper()
	zone := models.Zone{
		Name:       "Test Zone",
		Location:   "Nairobi",
		RouterName: "test-router",
		RouterIP:   "127.0.0.1",
	}
	if err := config.DB.Create(&zone).Error; err != nil {
		t.Fatalf("failed to create zone: %v", err)
	}
	pkg := models.Package{
		ZoneID:            zone.ID,
		Name:              "Standard",
		Type:              "hotspot",
		Price:             30,
		SpeedUploadKbps:   2048,
		SpeedDownloadKbps: 5120,
		BillingCycle:      "daily",
		Status:            "active",
	}
	if err := config.DB.Create(&pkg).Error; err != nil {
		t.Fatalf("failed to create package: %v", err)
	}
	return zone.ID, pkg.ID
}

// successCallbackPayload builds a Daraja-shaped successful STK callback payload.
func successCallbackPayload(checkoutID string, amount float64, receipt, phone string) map[string]interface{} {
	return map[string]interface{}{
		"Body": map[string]interface{}{
			"stkCallback": map[string]interface{}{
				"MerchantRequestID": "merchant-1",
				"CheckoutRequestID": checkoutID,
				"ResultCode":        float64(0),
				"ResultDesc":        "The service request is processed successfully.",
				"CallbackMetadata": map[string]interface{}{
					"Item": []interface{}{
						map[string]interface{}{"Name": "Amount", "Value": amount},
						map[string]interface{}{"Name": "MpesaReceiptNumber", "Value": receipt},
						map[string]interface{}{"Name": "TransactionDate", "Value": "20250101120000"},
						map[string]interface{}{"Name": "PhoneNumber", "Value": phone},
					},
				},
			},
		},
	}
}

// failureCallbackPayload builds a Daraja-shaped failed/cancelled STK callback payload.
func failureCallbackPayload(checkoutID string, resultCode float64, desc string) map[string]interface{} {
	return map[string]interface{}{
		"Body": map[string]interface{}{
			"stkCallback": map[string]interface{}{
				"MerchantRequestID": "merchant-1",
				"CheckoutRequestID": checkoutID,
				"ResultCode":        resultCode,
				"ResultDesc":        desc,
			},
		},
	}
}

func TestHandleCallback_Success(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()

	checkoutID := "ws_CO_test_success"
	txID := checkoutID
	payment := models.Payment{
		ZoneID:             zoneID,
		PackageID:          &pkgID,
		Phone:              "254712345678",
		Amount:             30,
		Currency:           "KES",
		Method:             "mpesa",
		Status:             "pending",
		MpesaTransactionID: &txID,
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	err := svc.HandleCallback(successCallbackPayload(checkoutID, 30, "RCT12345", "254712345678"))
	if err != nil {
		t.Fatalf("HandleCallback returned error: %v", err)
	}

	var reloaded models.Payment
	if err := config.DB.First(&reloaded, payment.ID).Error; err != nil {
		t.Fatalf("failed to reload payment: %v", err)
	}
	if reloaded.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", reloaded.Status)
	}
	if reloaded.MpesaReceiptNumber == nil || *reloaded.MpesaReceiptNumber != "RCT12345" {
		t.Errorf("expected receipt number 'RCT12345', got %v", reloaded.MpesaReceiptNumber)
	}
}

func TestHandleCallback_Failure(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()

	checkoutID := "ws_CO_test_failure"
	payment := models.Payment{
		ZoneID:             zoneID,
		PackageID:          &pkgID,
		Phone:              "254712345678",
		Amount:             30,
		Currency:           "KES",
		Method:             "mpesa",
		Status:             "pending",
		MpesaTransactionID: &checkoutID,
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	err := svc.HandleCallback(failureCallbackPayload(checkoutID, 1032, "Request cancelled by user"))
	if err != nil {
		t.Fatalf("HandleCallback returned error: %v", err)
	}

	var reloaded models.Payment
	config.DB.First(&reloaded, payment.ID)
	if reloaded.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", reloaded.Status)
	}
	if reloaded.StatusReason == nil || *reloaded.StatusReason != "Request cancelled by user" {
		t.Errorf("expected status_reason to be set, got %v", reloaded.StatusReason)
	}
}

func TestHandleCallback_PaymentNotFound(t *testing.T) {
	setupTestDB(t)
	svc := newTestMpesaService()

	err := svc.HandleCallback(successCallbackPayload("unknown-checkout-id", 30, "RCT1", "254712345678"))
	if err == nil {
		t.Fatal("expected an error for an unknown CheckoutRequestID, got nil")
	}
}

func TestHandleCallback_MalformedPayload(t *testing.T) {
	setupTestDB(t)
	svc := newTestMpesaService()

	if err := svc.HandleCallback(map[string]interface{}{"unexpected": "shape"}); err == nil {
		t.Fatal("expected an error for a malformed callback payload, got nil")
	}
}

// TestHandleCallback_DuplicateSuccess_IsIdempotent is the core regression test
// for the bug where Safaricom redelivering the same successful callback would
// re-run the whole success path: double-extending a customer's subscription
// and sending a second "payment received" SMS.
func TestHandleCallback_DuplicateSuccess_IsIdempotent(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()

	customer := models.Customer{
		Name:      "Jane Test",
		Phone:     "254712345678",
		ZoneID:    zoneID,
		PackageID: pkgID,
		Type:      "hotspot",
		Status:    "expired",
	}
	if err := config.DB.Create(&customer).Error; err != nil {
		t.Fatalf("failed to create customer: %v", err)
	}

	checkoutID := "ws_CO_test_duplicate"
	payment := models.Payment{
		CustomerID:         &customer.ID,
		ZoneID:             zoneID,
		PackageID:          &pkgID,
		Phone:              customer.Phone,
		Amount:             30,
		Currency:           "KES",
		Method:             "mpesa",
		Status:             "pending",
		MpesaTransactionID: &checkoutID,
	}
	if err := config.DB.Create(&payment).Error; err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	payload := successCallbackPayload(checkoutID, 30, "RCT99999", customer.Phone)

	// First delivery: should activate the customer and set an expiry.
	if err := svc.HandleCallback(payload); err != nil {
		t.Fatalf("first HandleCallback call failed: %v", err)
	}
	var afterFirst models.Customer
	config.DB.First(&afterFirst, customer.ID)
	if afterFirst.Status != "active" {
		t.Fatalf("expected customer status 'active' after first callback, got %q", afterFirst.Status)
	}
	if afterFirst.ExpiresAt == nil {
		t.Fatal("expected expires_at to be set after first callback")
	}
	firstExpiry := *afterFirst.ExpiresAt

	// Second delivery of the *same* callback (Safaricom redelivery): must be a no-op.
	if err := svc.HandleCallback(payload); err != nil {
		t.Fatalf("second HandleCallback call failed: %v", err)
	}
	var afterSecond models.Customer
	config.DB.First(&afterSecond, customer.ID)
	if afterSecond.ExpiresAt == nil {
		t.Fatal("expected expires_at to still be set after duplicate callback")
	}
	if !afterSecond.ExpiresAt.Equal(firstExpiry) {
		t.Errorf("duplicate callback extended expiry again: first=%v second=%v", firstExpiry, *afterSecond.ExpiresAt)
	}

	var paymentCount int64
	config.DB.Model(&models.Payment{}).Where("mpesa_transaction_id = ?", checkoutID).Count(&paymentCount)
	if paymentCount != 1 {
		t.Errorf("expected exactly 1 payment record, got %d", paymentCount)
	}
}

// TestHandleCallback_DuplicateFailure_IsIdempotent ensures a late/duplicate
// failure callback can't clobber a payment that's already completed.
func TestHandleCallback_DuplicateFailure_IsIdempotent(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()

	checkoutID := "ws_CO_test_late_failure"
	payment := models.Payment{
		ZoneID:             zoneID,
		PackageID:          &pkgID,
		Phone:              "254712345678",
		Amount:             30,
		Currency:           "KES",
		Method:             "mpesa",
		Status:             "pending",
		MpesaTransactionID: &checkoutID,
	}
	config.DB.Create(&payment)

	// Payment already completed (e.g. the success callback arrived first).
	if err := svc.HandleCallback(successCallbackPayload(checkoutID, 30, "RCT1", "254712345678")); err != nil {
		t.Fatalf("setup success callback failed: %v", err)
	}

	// A late failure callback for the same CheckoutRequestID must not flip
	// the now-completed payment back to "failed".
	if err := svc.HandleCallback(failureCallbackPayload(checkoutID, 1037, "Timeout")); err != nil {
		t.Fatalf("late failure callback returned error: %v", err)
	}

	var reloaded models.Payment
	config.DB.First(&reloaded, payment.ID)
	if reloaded.Status != "completed" {
		t.Errorf("late failure callback overwrote a completed payment: status=%q", reloaded.Status)
	}
}

func TestHandleCallback_VoucherFlow_DuplicateDoesNotReuseVoucher(t *testing.T) {
	setupTestDB(t)
	zoneID, pkgID := seedZone(t)
	svc := newTestMpesaService()

	voucher := models.Voucher{
		Code:      "ABCD1234",
		ZoneID:    zoneID,
		PackageID: pkgID,
		Type:      "single_use",
		Status:    "depleted", // simulates a voucher already marked depleted/reserved at STK push time
	}
	if err := config.DB.Create(&voucher).Error; err != nil {
		t.Fatalf("failed to create voucher: %v", err)
	}

	checkoutID := "ws_CO_test_voucher"
	payment := models.Payment{
		VoucherID:          &voucher.ID,
		ZoneID:             zoneID,
		PackageID:          &pkgID,
		Phone:              "254712345678",
		Amount:             30,
		Currency:           "KES",
		Method:             "mpesa",
		Status:             "pending",
		MpesaTransactionID: &checkoutID,
	}
	config.DB.Create(&payment)

	payload := successCallbackPayload(checkoutID, 30, "RCT1", "254712345678")
	if err := svc.HandleCallback(payload); err != nil {
		t.Fatalf("first callback failed: %v", err)
	}
	if err := svc.HandleCallback(payload); err != nil {
		t.Fatalf("duplicate callback failed: %v", err)
	}

	var reloaded models.Voucher
	config.DB.First(&reloaded, voucher.ID)
	if reloaded.Status != "unused" {
		t.Errorf("expected voucher status 'unused', got %q", reloaded.Status)
	}

	var completedCount int64
	config.DB.Model(&models.Payment{}).Where("mpesa_transaction_id = ? AND status = ?", checkoutID, "completed").Count(&completedCount)
	if completedCount != 1 {
		t.Errorf("expected exactly 1 completed payment, got %d", completedCount)
	}
}
