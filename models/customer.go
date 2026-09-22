package models

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Customer represents a PPPoE or Hotspot internet subscriber.
type Customer struct {
	ID            uint           `gorm:"primaryKey" json:"id"`
	Name          string         `gorm:"size:255;not null" json:"name"`
	Phone         string         `gorm:"size:20;not null;index" json:"phone"`
	Email         *string        `gorm:"size:255" json:"email"`
	ZoneID        uint           `gorm:"not null;index" json:"zone_id"`
	PackageID     uint           `gorm:"not null" json:"package_id"`
	Type          string         `gorm:"size:20;not null" json:"type"` // hotspot | pppoe
	PPPoEUsername *string        `gorm:"size:255;column:pppoe_username;index" json:"pppoe_username"`
	PPPoEPassword *string        `gorm:"size:255;column:pppoe_password" json:"pppoe_password"`
	Status        string         `gorm:"size:20;default:active;index:idx_customer_status_expiry,priority:1" json:"status"` // active|suspended|expired
	AccountNumber string         `gorm:"size:100;uniqueIndex" json:"account_number"`
	MacAddress    *string        `gorm:"size:45" json:"mac_address"`
	ExpiresAt     *time.Time     `gorm:"index:idx_customer_status_expiry,priority:2" json:"expires_at"`
	CreditBalance float64        `gorm:"type:decimal(10,2);default:0" json:"credit_balance"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"deleted_at,omitempty"`

	Zone    *Zone    `gorm:"foreignKey:ZoneID" json:"zone,omitempty"`
	Package *Package `gorm:"foreignKey:PackageID" json:"package,omitempty"`
}

func (Customer) TableName() string { return "customers" }

// BeforeCreate is a GORM hook that runs before a record is created.
func (c *Customer) BeforeCreate(tx *gorm.DB) (err error) {
	if c.AccountNumber == "" {
		if c.Phone != "" {
			// ZYR#<phone>, unless another ISP already has that phone as a
			// customer (account numbers are unique system-wide): then suffix
			// the zone so this ISP can still register them.
			c.AccountNumber = fmt.Sprintf("ZYR#%s", c.Phone)
			taken := func(n string) bool {
				var cnt int64
				tx.Session(&gorm.Session{NewDB: true}).Unscoped().Model(&Customer{}).Where("account_number = ?", n).Count(&cnt)
				return cnt > 0
			}
			if taken(c.AccountNumber) {
				base := c.AccountNumber
				c.AccountNumber = fmt.Sprintf("%s-%d", base, c.ZoneID)
				for i := 2; taken(c.AccountNumber); i++ {
					c.AccountNumber = fmt.Sprintf("%s-%d-%d", base, c.ZoneID, i)
				}
			}
		} else {
			var count int64
			tx.Unscoped().Model(&Customer{}).Where("account_number LIKE ? AND account_number NOT LIKE ?", "ZYR#%", "ZYR#0%").Count(&count)
			c.AccountNumber = fmt.Sprintf("ZYR#%d", 10001+count)
		}
	}
	return nil
}
