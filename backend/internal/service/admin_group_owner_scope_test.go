//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateModelRoutingOwnerRejectsCrossOwnerAccounts(t *testing.T) {
	ownerA := int64(7)
	ownerB := int64(8)
	repo := &accountRepoStub{accountsByID: map[int64]*Account{
		11: {ID: 11, OwnerUserID: &ownerA},
		12: {ID: 12, OwnerUserID: &ownerB},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	err := svc.validateModelRoutingOwner(context.Background(), &ownerA, map[string][]int64{"gpt-*": {11, 12}})
	require.ErrorContains(t, err, "owner does not match")
}

func TestValidateModelRoutingOwnerAllowsSameOwnerAccounts(t *testing.T) {
	owner := int64(7)
	repo := &accountRepoStub{accountsByID: map[int64]*Account{
		11: {ID: 11, OwnerUserID: &owner},
		12: {ID: 12, OwnerUserID: &owner},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	err := svc.validateModelRoutingOwner(context.Background(), &owner, map[string][]int64{"gpt-*": {11, 12}})
	require.NoError(t, err)
}

func TestValidateModelRoutingOwnerRejectsUserAccountForSystemGroup(t *testing.T) {
	owner := int64(7)
	repo := &accountRepoStub{accountsByID: map[int64]*Account{
		11: {ID: 11, OwnerUserID: &owner},
	}}
	svc := &adminServiceImpl{accountRepo: repo}

	err := svc.validateModelRoutingOwner(context.Background(), nil, map[string][]int64{"gpt-*": {11}})
	require.ErrorContains(t, err, "owner does not match")
}
