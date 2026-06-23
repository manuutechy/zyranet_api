package services

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"

	"github.com/zyranet/zyranet-api/config"
	"github.com/zyranet/zyranet-api/models"
)

type EmailService struct{}

func NewEmailService() *EmailService { return &EmailService{} }

func (s *EmailService) Send(to, subject, bodyHTML string) error {
	host := s.getSetting("smtp_host", "smtp.gmail.com")
	port := s.getSetting("smtp_port", "587")
	username := s.getSetting("smtp_username", "")
	password := s.getSetting("smtp_password", "")
	from := s.getSetting("smtp_from", username)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s",
		from, to, subject, bodyHTML)

	if username == "" || password == "" {
		log.Printf("[EMAIL] Mock: to=%s subject=%s msgLength=%d", to, subject, len(bodyHTML))
		return nil
	}

	auth := smtp.PlainAuth("", username, password, host)
	addr := fmt.Sprintf("%s:%s", host, port)
	err := smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
	if err != nil {
		log.Printf("[EMAIL] Error sending mail to %s: %v", to, err)
		return err
	}
	return nil
}

func (s *EmailService) getSetting(key, defaultVal string) string {
	var setting models.Setting
	if err := config.DB.Where("`key` = ?", key).First(&setting).Error; err == nil && setting.Value != nil {
		v := strings.TrimSpace(*setting.Value)
		if v != "" {
			return v
		}
	}
	return strings.TrimSpace(defaultVal)
}
