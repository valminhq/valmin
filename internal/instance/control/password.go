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
	return DecryptStoredPassword(keeper, instanceID, envelope)
}

// DecryptStoredPassword decrypts a password envelope already read from the instance's row.
func DecryptStoredPassword(keeper *crypto.Keeper, instanceID, envelope string) (string, error) {
	plaintext, err := keeper.Decrypt(
		crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(instanceID), envelope)
	if err != nil {
		return "", fmt.Errorf("decrypt password for instance %s: %w", instanceID, err)
	}
	return string(plaintext), nil
}
