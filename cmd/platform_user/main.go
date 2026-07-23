// Command platform_user creates or updates a Zyra Net Super Admin (SA)
// platform account. There is no self-registration UI for platform users by
// design, so the first (and any subsequent) SA account is provisioned here.
//
// Usage:
//
//	go run ./cmd/platform_user -name="Jane SA" -email=jane@zyranet.co.ke -password=ChangeMe123
package main

import (
	"flag"
	"log"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	name := flag.String("name", "", "Full name of the platform user")
	email := flag.String("email", "", "Login email")
	password := flag.String("password", "", "Login password")
	flag.Parse()

	if *email == "" || *password == "" {
		log.Fatal("[platform_user] -email and -password are required")
	}

	config.Load()
	config.ConnectDatabase()
	db := config.DB

	if err := db.AutoMigrate(&models.PlatformUser{}); err != nil {
		log.Fatalf("[platform_user] AutoMigrate failed: %v", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("[platform_user] bcrypt failed: %v", err)
	}

	displayName := *name
	if displayName == "" {
		displayName = "Platform Admin"
	}

	user := models.PlatformUser{
		Name:     displayName,
		Email:    *email,
		Password: string(hash),
		Status:   "active",
	}
	if err := db.Where(models.PlatformUser{Email: *email}).
		Assign(models.PlatformUser{Name: displayName, Password: string(hash), Status: "active"}).
		FirstOrCreate(&user).Error; err != nil {
		log.Fatalf("[platform_user] failed to create/update platform user: %v", err)
	}

	log.Printf("[platform_user] Ready — %s <%s> (id=%d)", user.Name, user.Email, user.ID)
}
