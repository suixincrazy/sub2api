package service

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type RedeemCode struct {
	ID          int64
	Code        string
	OwnerUserID *int64
	Type        string
	Value       float64
	Status      string
	UsedBy      *int64
	UsedAt      *time.Time
	Notes       string
	CreatedAt   time.Time
	ExpiresAt   *time.Time

	GroupID      *int64
	ValidityDays int
	MaxUses      int
	UsedCount    int

	User  *User
	Group *Group
}

func (r *RedeemCode) IsUsed() bool {
	return r.Status == StatusUsed || (r.MaxUses > 0 && r.UsedCount >= r.MaxUses)
}

func (r *RedeemCode) IsExpired() bool {
	return r.IsExpiredAt(time.Now())
}

func (r *RedeemCode) IsExpiredAt(now time.Time) bool {
	if r == nil {
		return false
	}
	if r.Status == StatusExpired {
		return true
	}
	return r.Status == StatusUnused && r.ExpiresAt != nil && !r.ExpiresAt.After(now)
}

func (r *RedeemCode) CanUse() bool {
	maxUses := r.MaxUses
	if maxUses <= 0 {
		maxUses = 1
	}
	return r.Status == StatusUnused && r.UsedCount < maxUses && !r.IsExpired()
}

func GenerateRedeemCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
