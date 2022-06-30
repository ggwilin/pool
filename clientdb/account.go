package clientdb

import (
	"bytes"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/pool/account"
	"go.etcd.io/bbolt"
)

const (
	// accountStateVersionedMask is a bit mask for detecting from the state
	// of an account whether that account has a version field encoded with
	// it or not. We use the first bit of the uint8 state field because we
	// are unlikely to ever have more than 127 different states.
	accountStateVersionedMask account.State = 0b1000_0000
)

var (
	// accountBucketKey is the top level bucket where we can find all
	// information about complete accounts. These accounts are indexed by
	// their trader key locator.
	accountBucketKey = []byte("account")

	// ErrAccountNotFound is an error returned when we attempt to retrieve
	// information about an account but it is not found.
	ErrAccountNotFound = errors.New("account not found")
)

// isVersioned returns true if the version bit is set in the given account
// state.
func isVersioned(state account.State) bool {
	return state&accountStateVersionedMask == accountStateVersionedMask
}

// setVersionBit sets the version bit in the given account state.
func setVersionBit(state account.State) account.State {
	return state | accountStateVersionedMask
}

// clearVersionBit clears the version bit in the given account state.
func clearVersionBit(state account.State) account.State {
	// The &^ operator means AND NOT, also known as the Bitclear operator.
	return state &^ accountStateVersionedMask
}

// getAccountKey returns the key for an account which is not partial.
func getAccountKey(account *account.Account) []byte {
	return account.TraderKey.PubKey.SerializeCompressed()
}

// AddAccount adds a record for the account to the database.
func (db *DB) AddAccount(account *account.Account) error {
	return db.Update(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		return storeAccount(accounts, account)
	})
}

// UpdateAccount updates an account in the database according to the given
// modifiers.
func (db *DB) UpdateAccount(acct *account.Account,
	modifiers ...account.Modifier) error {

	err := db.Update(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}
		accountKey := getAccountKey(acct)
		_, err = updateAccount(accounts, accounts, accountKey, modifiers)
		return err
	})
	if err != nil {
		return err
	}

	for _, modifier := range modifiers {
		modifier(acct)
	}

	return nil
}

// updateAccount reads an account from the src bucket, applies the given
// modifiers to it, and store it back into dst bucket.
func updateAccount(src, dst *bbolt.Bucket, accountKey []byte,
	modifiers []account.Modifier) (*account.Account, error) {

	dbAccount, err := readAccount(src, accountKey)
	if err != nil {
		return nil, err
	}

	for _, modifier := range modifiers {
		modifier(dbAccount)
	}

	return dbAccount, storeAccount(dst, dbAccount)
}

// Account retrieves a specific account by trader key or returns
// ErrAccountNotFound if it's not found.
func (db *DB) Account(traderKey *btcec.PublicKey) (*account.Account, error) {
	var acct *account.Account
	err := db.View(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		acct, err = readAccount(
			accounts, traderKey.SerializeCompressed(),
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return acct, nil
}

// Accounts retrieves all known accounts from the database.
func (db *DB) Accounts() ([]*account.Account, error) {
	var res []*account.Account
	err := db.View(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		return accounts.ForEach(func(k, v []byte) error {
			// We'll also get buckets here, skip those (identified
			// by nil value).
			if v == nil {
				return nil
			}

			acct, err := readAccount(accounts, k)
			if err != nil {
				return err
			}
			res = append(res, acct)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func storeAccount(targetBucket *bbolt.Bucket, a *account.Account) error {
	accountKey := getAccountKey(a)

	var accountBuf bytes.Buffer
	if err := serializeAccount(&accountBuf, a); err != nil {
		return err
	}

	return targetBucket.Put(accountKey, accountBuf.Bytes())
}

func readAccount(sourceBucket *bbolt.Bucket,
	accountKey []byte) (*account.Account, error) {

	accountBytes := sourceBucket.Get(accountKey)
	if accountBytes == nil {
		return nil, ErrAccountNotFound
	}

	return deserializeAccount(bytes.NewReader(accountBytes))
}

func serializeAccount(w *bytes.Buffer, a *account.Account) error {
	rawState := a.State
	accountIsVersioned := a.Version > account.VersionInitialNoVersion
	if accountIsVersioned {
		rawState = setVersionBit(rawState)
	}

	err := WriteElements(
		w, a.Value, a.Expiry, a.TraderKey, a.AuctioneerKey, a.BatchKey,
		a.Secret, rawState, a.HeightHint, a.OutPoint,
	)
	if err != nil {
		return err
	}

	// The latest transaction is not found within StateInitiated and
	// StateCanceledAfterRecovery.
	switch a.State {
	case account.StateInitiated, account.StateCanceledAfterRecovery:

	default:
		if err := WriteElement(w, a.LatestTx); err != nil {
			return err
		}
	}

	// The version flag encoded within the state will inform the
	// de-serialize method that it should read another field. Therefore, we
	// can safely write it here.
	if accountIsVersioned {
		if err := WriteElement(w, a.Version); err != nil {
			return err
		}
	}

	return nil
}

func deserializeAccount(r io.Reader) (*account.Account, error) {
	var (
		a        account.Account
		rawState account.State
	)
	err := ReadElements(
		r, &a.Value, &a.Expiry, &a.TraderKey, &a.AuctioneerKey,
		&a.BatchKey, &a.Secret, &rawState, &a.HeightHint, &a.OutPoint,
	)
	if err != nil {
		return nil, err
	}

	// We might have a version flag encoded within the state. We want to
	// hide that internal mechanism from the caller, so we need to remove
	// the flag again.
	a.State = clearVersionBit(rawState)

	// The latest transaction is not found within StateInitiated and
	// StateCanceledAfterRecovery.
	switch a.State {
	case account.StateInitiated, account.StateCanceledAfterRecovery:

	default:
		if err := ReadElement(r, &a.LatestTx); err != nil {
			return nil, err
		}
	}

	// If there was a version flag, we know we're supposed to read another
	// field here.
	if isVersioned(rawState) {
		if err := ReadElement(r, &a.Version); err != nil {
			return nil, err
		}
	}

	return &a, nil
}
