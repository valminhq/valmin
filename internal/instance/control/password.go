package control

import (
	"context"
	"fmt"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/store"
)

// DecryptPassword reads and decrypts an instance's stored server password.
func DecryptPassword(ctx context.Context, db *store.DB, keeper *crypto.Keeper, instanceID string) (string, error) {
	envelope, err := db.InstancePassword(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("read encrypted password for instance %s: %w", instanceID, err)
	}
	return decryptStoredPassword(keeper, instanceID, envelope)
}

// DecryptBranchPassword reads and decrypts the password of an instance's Steam branch, or ""
// when the branch has none.
func DecryptBranchPassword(
	ctx context.Context, db *store.DB, keeper *crypto.Keeper, instanceID string,
) (string, error) {
	envelope, err := db.InstanceBranchPassword(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("read encrypted branch password for instance %s: %w", instanceID, err)
	}
	if envelope == "" {
		return "", nil
	}
	plaintext, err := keeper.Decrypt(
		crypto.PurposeBranchPassword, crypto.BranchPasswordLocation(instanceID), envelope)
	if err != nil {
		return "", fmt.Errorf("decrypt branch password for instance %s: %w", instanceID, err)
	}
	return string(plaintext), nil
}

// decryptStoredPassword decrypts a password envelope already read from the instance's row.
func decryptStoredPassword(keeper *crypto.Keeper, instanceID, envelope string) (string, error) {
	plaintext, err := keeper.Decrypt(
		crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(instanceID), envelope)
	if err != nil {
		return "", fmt.Errorf("decrypt password for instance %s: %w", instanceID, err)
	}
	return string(plaintext), nil
}
