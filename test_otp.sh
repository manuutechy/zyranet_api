#!/usr/bin/env bash
# test_otp.sh — Test client OTP request and verification flow
# Ensure the Go API server is running on http://127.0.0.1:8000 before executing.

set -e

API_URL="http://127.0.0.1:8000/api/v1"
TEST_PHONE="${1:-0712345678}"

echo "=================================================="
echo " Zyra Net API — OTP Integration Test Tool"
echo "=================================================="
echo ""

# 1. Check if Go server is running
echo "Checking API connection..."
if ! curl -s --connect-timeout 2 "http://127.0.0.1:8000/health" > /dev/null; then
    echo "ERROR: Go API server is not running on http://127.0.0.1:8000."
    echo "Please start the server first using: go run main.go"
    exit 1
fi
echo "✓ Connected to API."
echo ""

# 2. Trigger OTP SMS Request
echo "1. Requesting OTP code for ${TEST_PHONE}..."
OTP_RESP=$(curl -s -X POST "${API_URL}/customer/auth/otp" \
  -H "Content-Type: application/json" \
  -d "{\"phone\": \"${TEST_PHONE}\"}")

echo "Response from server:"
echo "${OTP_RESP}" | grep -q "success" && echo "✓ Success!" || echo "✗ Failed!"
echo "${OTP_RESP}"
echo ""

# 3. Verify OTP using Sandbox Bypass Code
echo "2. Verifying OTP using sandbox bypass code (1234)..."
VERIFY_RESP=$(curl -s -X POST "${API_URL}/customer/auth/verify" \
  -H "Content-Type: application/json" \
  -d "{\"phone\": \"${TEST_PHONE}\", \"otp\": \"1234\"}")

echo "Response from server:"
echo "${VERIFY_RESP}" | grep -q "token" && echo "✓ Verification Successful!" || echo "✗ Verification Failed!"
echo "${VERIFY_RESP}"
echo ""

echo "=================================================="
echo " Test completed successfully."
echo "=================================================="
