package service

import "time"

type RedeemCodeUsage struct {
	ID           int64
	RedeemCodeID int64
	UserID       int64
	UsedAt       time.Time
	User         *User
}
