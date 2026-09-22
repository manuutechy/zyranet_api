package services

import (
	"strings"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"gorm.io/gorm"
)

// sameISPZones is a sub-query of every zone that belongs to the same ISP as
// zoneID. Customer lookups by phone or MAC must stay inside it: the same phone
// or device can be a customer of two ISPs, and matching the other ISP's record
// would attach a payment to — and move — the wrong ISP's customer.
func sameISPZones(zoneID uint) *gorm.DB {
	return config.DB.Model(&models.Zone{}).Select("id").Where(
		"organization_id = (?)", config.DB.Model(&models.Zone{}).Select("organization_id").Where("id = ?", zoneID))
}

// CustomerByPhoneForZone finds a customer with this phone in the same ISP as zoneID.
func CustomerByPhoneForZone(zoneID uint, phone string) (*models.Customer, bool) {
	phone = strings.TrimSpace(phone)
	if phone == "" || zoneID == 0 {
		return nil, false
	}
	var c models.Customer
	if err := config.DB.Where("phone = ? AND zone_id IN (?)", phone, sameISPZones(zoneID)).First(&c).Error; err != nil {
		return nil, false
	}
	return &c, true
}

// CustomerByDeviceForZone finds the customer owning this device MAC in the same ISP as zoneID.
func CustomerByDeviceForZone(zoneID uint, mac string) (*models.Customer, bool) {
	mac = strings.TrimSpace(mac)
	if mac == "" || zoneID == 0 {
		return nil, false
	}
	var dev models.CustomerDevice
	err := config.DB.Preload("Customer").
		Joins("JOIN customers ON customers.id = customer_devices.customer_id").
		Where("customer_devices.mac_address = ? AND customers.zone_id IN (?)", mac, sameISPZones(zoneID)).
		First(&dev).Error
	if err != nil || dev.Customer == nil {
		return nil, false
	}
	return dev.Customer, true
}

// CustomerBelongsToZoneISP reports whether customerID is a customer of the
// ISP that owns zoneID.
func CustomerBelongsToZoneISP(customerID, zoneID uint) bool {
	var n int64
	config.DB.Model(&models.Customer{}).Where("id = ? AND zone_id IN (?)", customerID, sameISPZones(zoneID)).Count(&n)
	return n > 0
}
