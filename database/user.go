package database

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SkynetLabs/skynet-accounts/build"
	"github.com/SkynetLabs/skynet-accounts/hash"
	"github.com/SkynetLabs/skynet-accounts/jwt"
	"github.com/SkynetLabs/skynet-accounts/lib"
	"github.com/SkynetLabs/skynet-accounts/skynet"
	"github.com/SkynetLabs/skynet-accounts/types"

	"gitlab.com/NebulousLabs/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

const (
	// TierAnonymous reserved
	TierAnonymous = iota
	// TierFree free
	TierFree
	// TierPremium5 5
	TierPremium5
	// TierPremium20 20
	TierPremium20
	// TierPremium80 80
	TierPremium80
	// TierMaxReserved is a guard value that helps us validate tier values.
	TierMaxReserved

	// mbpsToBytesPerSecond is a multiplier to get from megabits per second to
	// bytes per second.
	mbpsToBytesPerSecond = 1024 * 1024 / 8

	// filesAllowedPerTB defines a limit of number of uploaded files we impose
	// on users. While we define it per TB, we impose it based on their entire
	// quota, so an Extreme user will be able to upload up to 400_000 files
	// before being hit with a speed limit.
	filesAllowedPerTB = 25_000
)

var (
	// True is a helper for when we need to pass a *bool to MongoDB.
	True = true
	// UserLimits defines the speed limits for each tier.
	// RegistryDelay delay is in ms.
	UserLimits = map[int]TierLimits{
		TierAnonymous: {
			TierName:          "anonymous",
			UploadBandwidth:   5 * mbpsToBytesPerSecond,
			DownloadBandwidth: 20 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.GiB,
			MaxNumberUploads:  0,
			RegistryDelay:     250,
			Storage:           0,
		},
		TierFree: {
			TierName:          "free",
			UploadBandwidth:   10 * mbpsToBytesPerSecond,
			DownloadBandwidth: 40 * mbpsToBytesPerSecond,
			MaxUploadSize:     100 * skynet.GiB,
			MaxNumberUploads:  0.1 * filesAllowedPerTB,
			RegistryDelay:     125,
			Storage:           100 * skynet.GiB,
		},
		TierPremium5: {
			TierName:          "plus",
			UploadBandwidth:   20 * mbpsToBytesPerSecond,
			DownloadBandwidth: 80 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.TiB,
			MaxNumberUploads:  1 * filesAllowedPerTB,
			RegistryDelay:     0,
			Storage:           1 * skynet.TiB,
		},
		TierPremium20: {
			TierName:          "pro",
			UploadBandwidth:   40 * mbpsToBytesPerSecond,
			DownloadBandwidth: 160 * mbpsToBytesPerSecond,
			MaxUploadSize:     4 * skynet.TiB,
			MaxNumberUploads:  4 * filesAllowedPerTB,
			RegistryDelay:     0,
			Storage:           4 * skynet.TiB,
		},
		TierPremium80: {
			TierName:          "extreme",
			UploadBandwidth:   80 * mbpsToBytesPerSecond,
			DownloadBandwidth: 320 * mbpsToBytesPerSecond,
			MaxUploadSize:     10 * skynet.TiB,
			MaxNumberUploads:  20 * filesAllowedPerTB,
			RegistryDelay:     0,
			Storage:           20 * skynet.TiB,
		},
	}

	// ErrInvalidToken is returned when the token is found to be invalid for any
	// reason, including expiration.
	ErrInvalidToken = errors.New("invalid token")
)

type (
	// User represents a Skynet user.
	User struct {
		// ID is auto-generated by Mongo on insert. We will usually use it in
		// its ID.Hex() form.
		ID                               primitive.ObjectID `bson:"_id,omitempty" json:"-"`
		Email                            types.EmailField   `bson:"email" json:"email"`
		EmailConfirmationToken           string             `bson:"email_confirmation_token,omitempty" json:"-"`
		EmailConfirmationTokenExpiration time.Time          `bson:"email_confirmation_token_expiration,omitempty" json:"-"`
		PasswordHash                     string             `bson:"password_hash" json:"-"`
		RecoveryToken                    string             `bson:"recovery_token,omitempty" json:"-"`
		Sub                              string             `bson:"sub" json:"sub"`
		Tier                             int                `bson:"tier" json:"tier"`
		SubscribedUntil                  time.Time          `bson:"subscribed_until" json:"subscribedUntil"`
		SubscriptionStatus               string             `bson:"subscription_status" json:"subscriptionStatus"`
		SubscriptionCancelAt             time.Time          `bson:"subscription_cancel_at" json:"subscriptionCancelAt"`
		SubscriptionCancelAtPeriodEnd    bool               `bson:"subscription_cancel_at_period_end" json:"subscriptionCancelAtPeriodEnd"`
		StripeID                         string             `bson:"stripe_id" json:"stripeCustomerId"`
		QuotaExceeded                    bool               `bson:"quota_exceeded" json:"quotaExceeded"`
		// The currently active (or default) key is going to be the first one in
		// the list. If we want to activate a new pubkey, we'll just move it to
		// the first position in the list.
		PubKeys []PubKey `bson:"pub_keys" json:"-"`
	}
	// UserStats contains statistical information about the user.
	UserStats struct {
		RawStorageUsed     int64 `json:"rawStorageUsed"`
		NumRegReads        int64 `json:"numRegReads"`
		NumRegWrites       int64 `json:"numRegWrites"`
		NumUploads         int   `json:"numUploads"`
		NumDownloads       int   `json:"numDownloads"`
		TotalUploadsSize   int64 `json:"totalUploadsSize"`
		TotalDownloadsSize int64 `json:"totalDownloadsSize"`
		BandwidthUploads   int64 `json:"bwUploads"`
		BandwidthDownloads int64 `json:"bwDownloads"`
		BandwidthRegReads  int64 `json:"bwRegReads"`
		BandwidthRegWrites int64 `json:"bwRegWrites"`
	}
	// TierLimits defines the speed limits imposed on the user based on their
	// tier.
	TierLimits struct {
		TierName          string `json:"tierName"`
		UploadBandwidth   int    `json:"upload"`        // bytes per second
		DownloadBandwidth int    `json:"download"`      // bytes per second
		MaxUploadSize     int64  `json:"maxUploadSize"` // the max size of a single upload in bytes
		MaxNumberUploads  int    `json:"-"`
		RegistryDelay     int    `json:"registry"` // ms delay
		Storage           int64  `json:"-"`
	}
)

// UserByEmail returns the user with the given username.
func (db *DB) UserByEmail(ctx context.Context, email string) (*User, error) {
	users, err := db.managedUsersByField(ctx, "email", email)
	if err != nil {
		return nil, err
	}
	return users[0], nil
}

// UserByID finds a user by their ID.
func (db *DB) UserByID(ctx context.Context, id primitive.ObjectID) (*User, error) {
	filter := bson.D{{"_id", id}}
	c, err := db.staticUsers.Find(ctx, filter)
	if err != nil {
		return nil, errors.AddContext(err, "failed to Find")
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Debugln("Error on closing DB cursor.", errDef)
		}
	}()
	// Get the first result.
	if ok := c.Next(ctx); !ok {
		return nil, ErrUserNotFound
	}
	// Ensure there are no more results.
	if ok := c.Next(ctx); ok {
		build.Critical("more than one user found for id", id)
	}
	var u User
	err = c.Decode(&u)
	if err != nil {
		return nil, errors.AddContext(err, "failed to parse value from DB")
	}
	return &u, nil
}

// UserByPubKey returns the user with the given pubkey.
func (db *DB) UserByPubKey(ctx context.Context, pk PubKey) (*User, error) {
	sr := db.staticUsers.FindOne(ctx, bson.M{"pub_keys": pk})
	var u User
	err := sr.Decode(&u)
	if err != nil {
		return nil, ErrUserNotFound
	}
	return &u, nil
}

// UserByRecoveryToken returns the user with the given recovery token.
func (db *DB) UserByRecoveryToken(ctx context.Context, token string) (*User, error) {
	users, err := db.managedUsersByField(ctx, "recovery_token", token)
	if err != nil {
		return nil, err
	}
	return users[0], nil
}

// UserByStripeID finds a user by their Stripe customer id.
func (db *DB) UserByStripeID(ctx context.Context, id string) (*User, error) {
	filter := bson.D{{"stripe_id", id}}
	c, err := db.staticUsers.Find(ctx, filter)
	if err != nil {
		return nil, errors.AddContext(err, "failed to Find")
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Debugln("Error on closing DB cursor.", errDef)
		}
	}()
	// Get the first result.
	if ok := c.Next(ctx); !ok {
		return nil, ErrUserNotFound
	}
	// Ensure there are no more results.
	if ok := c.Next(ctx); ok {
		build.Critical(fmt.Sprintf("more than one user found for stripe customer id '%s'", id))
	}
	var u User
	err = c.Decode(&u)
	if err != nil {
		return nil, errors.AddContext(err, "failed to parse value from DB")
	}
	return &u, nil
}

// UserBySub returns the user with the given sub. If `create` is `true` it will
// create the user if it doesn't exist.
func (db *DB) UserBySub(ctx context.Context, sub string, create bool) (*User, error) {
	users, err := db.managedUsersBySub(ctx, sub)
	if create && errors.Contains(err, ErrUserNotFound) {
		_, email, err := jwt.UserDetailsFromJWT(ctx)
		if err != nil {
			// Log the error but don't do anything differently.
			db.staticLogger.Debugf("We failed to extract the expected user infotmation from the JWT token. Error: %s", err.Error())
		}
		u, err := db.UserCreate(ctx, email, "", sub, TierFree)
		// If we're successful or hit any error, other than a duplicate key we
		// want to just return. Hitting a duplicate key error means we ran into
		// a race condition and we can easily recover from that.
		if err == nil || !strings.Contains(err.Error(), "E11000 duplicate key error collection") {
			return u, err
		}
		// Recover from the race condition by fetching the existing user from
		// the DB.
		users, err = db.managedUsersBySub(ctx, sub)
	}
	if err != nil {
		return nil, err
	}
	return users[0], nil
}

// UserConfirmEmail confirms that the email to which the passed confirmation
// token belongs actually belongs to its user.
func (db *DB) UserConfirmEmail(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, errors.AddContext(ErrInvalidToken, "token cannot be empty")
	}
	users, err := db.managedUsersByField(ctx, "email_confirmation_token", token)
	if err != nil {
		return nil, errors.AddContext(err, "failed to read users from DB")
	}
	if len(users) == 0 {
		return nil, errors.AddContext(ErrInvalidToken, "no user has this token")
	}
	if len(users) > 1 {
		build.Critical("multiple users found for the same confirmation token", token)
		return nil, errors.AddContext(ErrInvalidToken, "please request a new token")
	}
	u := users[0]
	// Check if the token has expired.
	if u.EmailConfirmationTokenExpiration.Before(time.Now().UTC()) {
		return nil, errors.AddContext(ErrInvalidToken, "token expired")
	}
	u.EmailConfirmationToken = ""
	err = db.UserSave(ctx, u)
	if err != nil {
		return nil, errors.AddContext(err, "failed to update user")
	}
	return u, nil
}

// UserCreate creates a new user in the DB.
//
// The `sub` field is optional.
//
// The new user is created as "unconfirmed" and a confirmation email is sent to
// the address they provided.
func (db *DB) UserCreate(ctx context.Context, emailAddr, pass, sub string, tier int) (*User, error) {
	// TODO Uncomment once we no longer create users via the UserBySub and similar methods.
	// emailAddr, err := lib.NormalizeEmail(emailAddr)
	// if err != nil {
	// 	return nil, errors.AddContext(err, "invalid email address")
	// }
	// Check for an existing user with this email.
	users, err := db.managedUsersByField(ctx, "email", emailAddr)
	if err != nil && !errors.Contains(err, ErrUserNotFound) {
		return nil, errors.AddContext(err, "failed to query DB")
	}
	if len(users) > 0 {
		return nil, ErrUserAlreadyExists
	}
	if sub == "" {
		return nil, errors.New("empty sub is not allowed")
	}
	// Check for an existing user with this sub.
	users, err = db.managedUsersBySub(ctx, sub)
	if err != nil && !errors.Contains(err, ErrUserNotFound) {
		return nil, errors.AddContext(err, "failed to query DB")
	}
	if len(users) > 0 {
		return nil, ErrUserAlreadyExists
	}
	// TODO Review this when we fully migrate away from Kratos.
	// Generate a password hash, if a password is provided. A password might not
	// be provided if the user is generated externally, e.g. in Kratos. We can
	// remove that option in the future when `accounts` is the only system
	// managing users but for the moment we still need it.
	var passHash []byte
	if pass != "" {
		passHash, err = hash.Generate(pass)
		if err != nil {
			return nil, errors.AddContext(ErrGeneralInternalFailure, "failed to hash password")
		}
	}
	emailConfToken, err := lib.GenerateUUID()
	if err != nil {
		return nil, errors.AddContext(err, "failed to generate an email confirmation token")
	}
	u := &User{
		ID:                               primitive.ObjectID{},
		Email:                            types.EmailField(emailAddr),
		EmailConfirmationToken:           emailConfToken,
		EmailConfirmationTokenExpiration: time.Now().UTC().Add(EmailConfirmationTokenTTL).Truncate(time.Millisecond),
		PasswordHash:                     string(passHash),
		Sub:                              sub,
		Tier:                             tier,
	}
	// Insert the user.
	fields, err := bson.Marshal(u)
	if err != nil {
		return nil, err
	}
	ir, err := db.staticUsers.InsertOne(ctx, fields)
	if err != nil {
		return nil, errors.AddContext(err, "failed to Insert")
	}
	u.ID = ir.InsertedID.(primitive.ObjectID)
	return u, nil
}

// UserCreatePK creates a new user with a pubkey in the DB.
//
// The `pass` and `sub` fields are optional.
//
// The new user is created as "unconfirmed" and a confirmation email is sent to
// the address they provided.
func (db *DB) UserCreatePK(ctx context.Context, emailAddr, pass, sub string, pk PubKey, tier int) (*User, error) {
	// Validate and normalize the email.
	emailAddr, err := lib.NormalizeEmail(emailAddr)
	if err != nil {
		return nil, errors.AddContext(err, "invalid email address")
	}
	// Check for an existing user with this email.
	users, err := db.managedUsersByField(ctx, "email", emailAddr)
	if err != nil && !errors.Contains(err, ErrUserNotFound) {
		return nil, errors.AddContext(err, "failed to query DB")
	}
	if len(users) > 0 {
		return nil, ErrUserAlreadyExists
	}
	if sub == "" {
		sub, err = lib.GenerateUUID()
		if err != nil {
			return nil, errors.AddContext(err, "failed to generate user sub")
		}
	}
	// Check for an existing user with this sub.
	users, err = db.managedUsersBySub(ctx, sub)
	if err != nil && !errors.Contains(err, ErrUserNotFound) {
		return nil, errors.AddContext(err, "failed to query DB")
	}
	if len(users) > 0 {
		return nil, ErrUserAlreadyExists
	}
	// Generate a password hash, if a password is provided. A password might not
	// be provided if the user intends to only use pubkey authentication.
	var passHash []byte
	if pass != "" {
		passHash, err = hash.Generate(pass)
		if err != nil {
			return nil, errors.AddContext(ErrGeneralInternalFailure, "failed to hash password")
		}
	}
	emailConfToken, err := lib.GenerateUUID()
	if err != nil {
		return nil, errors.AddContext(err, "failed to generate an email confirmation token")
	}
	u := &User{
		ID:                               primitive.ObjectID{},
		Email:                            types.EmailField(emailAddr),
		EmailConfirmationToken:           emailConfToken,
		EmailConfirmationTokenExpiration: time.Now().UTC().Add(EmailConfirmationTokenTTL).Truncate(time.Millisecond),
		PasswordHash:                     string(passHash),
		Sub:                              sub,
		Tier:                             tier,
		PubKeys:                          []PubKey{pk},
	}
	// Insert the user.
	fields, err := bson.Marshal(u)
	if err != nil {
		return nil, err
	}
	ir, err := db.staticUsers.InsertOne(ctx, fields)
	if err != nil {
		return nil, errors.AddContext(err, "failed to Insert")
	}
	u.ID = ir.InsertedID.(primitive.ObjectID)
	return u, nil
}

// UserDelete deletes a user by their ID.
func (db *DB) UserDelete(ctx context.Context, u *User) error {
	if u.ID.IsZero() {
		return errors.AddContext(ErrUserNotFound, "user struct not fully initialised")
	}
	// Delete all data associated with this user.
	filter := bson.D{{"user_id", u.ID}}
	_, err := db.staticDownloads.DeleteMany(ctx, filter)
	if err != nil {
		return errors.AddContext(err, "failed to delete user downloads")
	}
	_, err = db.staticUploads.DeleteMany(ctx, filter)
	if err != nil {
		return errors.AddContext(err, "failed to delete user uploads")
	}
	_, err = db.staticRegistryReads.DeleteMany(ctx, filter)
	if err != nil {
		return errors.AddContext(err, "failed to delete user registry reads")
	}
	_, err = db.staticRegistryWrites.DeleteMany(ctx, filter)
	if err != nil {
		return errors.AddContext(err, "failed to delete user registry writes")
	}
	// Delete the actual user.
	filter = bson.D{{"_id", u.ID}}
	dr, err := db.staticUsers.DeleteOne(ctx, filter)
	if err != nil {
		return errors.AddContext(err, "failed to Delete")
	}
	if dr.DeletedCount == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UserSave saves the user to the DB.
func (db *DB) UserSave(ctx context.Context, u *User) error {
	filter := bson.M{"_id": u.ID}
	opts := &options.ReplaceOptions{
		Upsert: &True,
	}
	_, err := db.staticUsers.ReplaceOne(ctx, filter, u, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	return nil
}

// UserSetStripeID changes the user's stripe id in the DB.
func (db *DB) UserSetStripeID(ctx context.Context, u *User, stripeID string) error {
	filter := bson.M{"_id": u.ID}
	update := bson.M{"$set": bson.M{"stripe_id": stripeID}}
	opts := options.Update().SetUpsert(true)
	_, err := db.staticUsers.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	return nil
}

// UserSetTier sets the user's tier to the given value.
func (db *DB) UserSetTier(ctx context.Context, u *User, t int) error {
	if t <= TierAnonymous || t >= TierMaxReserved {
		return errors.New("invalid tier value")
	}
	filter := bson.M{"_id": u.ID}
	update := bson.M{"$set": bson.M{"tier": t}}
	opts := options.Update().SetUpsert(true)
	_, err := db.staticUsers.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	u.Tier = t
	return nil
}

// UserStats returns statistical information about the user.
func (db *DB) UserStats(ctx context.Context, user User) (*UserStats, error) {
	return db.userStats(ctx, user)
}

// Ping sends a ping command to verify that the client can connect to the DB and
// specifically to the primary.
func (db *DB) Ping(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.staticDB.Client().Ping(ctx2, readpref.Primary())
}

// managedUsersByField finds all users that have a given field value.
// The calling method is responsible for the validation of the value.
func (db *DB) managedUsersByField(ctx context.Context, fieldName, fieldValue string) ([]*User, error) {
	filter := bson.M{fieldName: fieldValue}
	c, err := db.staticUsers.Find(ctx, filter)
	if err != nil {
		return nil, errors.AddContext(err, "failed to find user")
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Debugln("Error on closing DB cursor.", errDef)
		}
	}()

	var users []*User
	for c.Next(ctx) {
		var u User
		if err = c.Decode(&u); err != nil {
			return nil, errors.AddContext(err, "failed to parse value from DB")
		}
		users = append(users, &u)
	}
	if len(users) == 0 {
		return users, ErrUserNotFound
	}
	return users, nil
}

// managedUsersBySub fetches all users that have the given sub. This should
// normally be up to one user.
func (db *DB) managedUsersBySub(ctx context.Context, sub string) ([]*User, error) {
	return db.managedUsersByField(ctx, "sub", sub)
}

// userStats reports statistical information about the user.
func (db *DB) userStats(ctx context.Context, user User) (*UserStats, error) {
	stats := UserStats{}
	var errs []error
	var errsMux sync.Mutex
	regErr := func(msg string, e error) {
		db.staticLogger.Infoln(msg, e)
		errsMux.Lock()
		errs = append(errs, e)
		errsMux.Unlock()
	}
	startOfMonth := monthStart(user.SubscribedUntil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, size, rawStorage, bw, err := db.UserUploadStats(ctx, user.ID, startOfMonth)
		if err != nil {
			regErr("Failed to get user's upload bandwidth used:", err)
			return
		}
		stats.NumUploads = n
		stats.TotalUploadsSize = size
		stats.RawStorageUsed = rawStorage
		stats.BandwidthUploads = bw
		db.staticLogger.Tracef("User %s upload bandwidth: %v", user.ID.Hex(), bw)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, size, bw, err := db.userDownloadStats(ctx, user.ID, startOfMonth)
		if err != nil {
			regErr("Failed to get user's download bandwidth used:", err)
			return
		}
		stats.NumDownloads = n
		stats.TotalDownloadsSize = size
		stats.BandwidthDownloads = bw
		db.staticLogger.Tracef("User %s download bandwidth: %v", user.ID.Hex(), bw)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, bw, err := db.userRegistryWriteStats(ctx, user.ID, startOfMonth)
		if err != nil {
			regErr("Failed to get user's registry write bandwidth used:", err)
			return
		}
		stats.NumRegWrites = n
		stats.BandwidthRegWrites = bw
		db.staticLogger.Tracef("User %s registry write bandwidth: %v", user.ID.Hex(), bw)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, bw, err := db.userRegistryReadStats(ctx, user.ID, startOfMonth)
		if err != nil {
			regErr("Failed to get user's registry read bandwidth used:", err)
			return
		}
		stats.NumRegReads = n
		stats.BandwidthRegReads = bw
		db.staticLogger.Tracef("User %s registry read bandwidth: %v", user.ID.Hex(), bw)
	}()

	wg.Wait()
	if len(errs) > 0 {
		return nil, errors.Compose(errs...)
	}
	return &stats, nil
}

// UserUploadStats reports on the user's uploads - count, total size and total
// bandwidth used. It uses the total size of the uploaded skyfiles as basis.
func (db *DB) UserUploadStats(ctx context.Context, id primitive.ObjectID, since time.Time) (count int, totalSize int64, rawStorageUsed int64, totalBandwidth int64, err error) {
	matchStage := bson.D{{"$match", bson.M{"user_id": id}}}
	lookupStage := bson.D{
		{"$lookup", bson.D{
			{"from", "skylinks"},
			{"localField", "skylink_id"},
			{"foreignField", "_id"},
			{"as", "skylink_data"},
		}},
	}
	replaceStage := bson.D{
		{"$replaceRoot", bson.D{
			{"newRoot", bson.D{
				{"$mergeObjects", bson.A{
					bson.D{{"$arrayElemAt", bson.A{"$skylink_data", 0}}}, "$$ROOT"},
				},
			}},
		}},
	}
	// These are the fields we don't need.
	projectStage := bson.D{{"$project", bson.D{
		{"_id", 0},
		{"user_id", 0},
		{"skylink_data", 0},
		{"name", 0},
		{"skylink_id", 0},
	}}}

	pipeline := mongo.Pipeline{matchStage, lookupStage, replaceStage, projectStage}
	c, err := db.staticUploads.Aggregate(ctx, pipeline)
	if err != nil {
		return
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()

	// We need this struct, so we can safely decode both int32 and int64.
	result := struct {
		Size      int64     `bson:"size"`
		Skylink   string    `bson:"skylink"`
		Unpinned  bool      `bson:"unpinned"`
		Timestamp time.Time `bson:"timestamp"`
	}{}
	processedSkylinks := make(map[string]bool)
	for c.Next(ctx) {
		if err = c.Decode(&result); err != nil {
			err = errors.AddContext(err, "failed to decode DB data")
			return
		}
		// We first weed out any old uploads that we fetch only in order to
		// calculate the total used storage.
		if result.Timestamp.Before(since) {
			if result.Unpinned || processedSkylinks[result.Skylink] {
				continue
			}
			processedSkylinks[result.Skylink] = true
			totalSize += result.Size
			continue
		}
		// All bandwidth is counted, regardless of unpinned status.
		totalBandwidth += skynet.BandwidthUploadCost(result.Size)
		// Count only uploads that are still pinned towards total count.
		if result.Unpinned {
			continue
		}
		count++
		// Count only unique uploads towards total size and used storage.
		if processedSkylinks[result.Skylink] {
			continue
		}
		processedSkylinks[result.Skylink] = true
		totalSize += result.Size
		rawStorageUsed += skynet.RawStorageUsed(result.Size)
	}
	return count, totalSize, rawStorageUsed, totalBandwidth, nil
}

// userDownloadStats reports on the user's downloads - count, total size and
// total bandwidth used. It uses the actual bandwidth used, as reported by nginx.
func (db *DB) userDownloadStats(ctx context.Context, id primitive.ObjectID, monthStart time.Time) (count int, totalSize int64, totalBandwidth int64, err error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", id},
		{"created_at", bson.D{{"$gt", monthStart}}},
	}}}
	lookupStage := bson.D{
		{"$lookup", bson.D{
			{"from", "skylinks"},
			{"localField", "skylink_id"}, // field in the downloads collection
			{"foreignField", "_id"},      // field in the skylinks collection
			{"as", "fromSkylinks"},
		}},
	}
	replaceStage := bson.D{
		{"$replaceRoot", bson.D{
			{"newRoot", bson.D{
				{"$mergeObjects", bson.A{
					bson.D{{"$arrayElemAt", bson.A{"$fromSkylinks", 0}}}, "$$ROOT"},
				},
			}},
		}},
	}
	// This stage checks if the download has a non-zero `bytes` field and if so,
	// it takes it as the download's size. Otherwise it reports the full
	// skylink's size as download's size.
	projectStage := bson.D{{"$project", bson.D{
		{"size", bson.D{
			{"$cond", bson.A{
				bson.D{{"$gt", bson.A{"$bytes", 0}}}, // if
				"$bytes",                             // then
				"$size",                              // else
			}},
		}},
	}}}

	pipeline := mongo.Pipeline{matchStage, lookupStage, replaceStage, projectStage}
	c, err := db.staticDownloads.Aggregate(ctx, pipeline)
	if err != nil {
		err = errors.AddContext(err, "DB query failed")
		return
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()

	// We need this struct, so we can safely decode both int32 and int64.
	result := struct {
		Size int64 `bson:"size"`
	}{}
	for c.Next(ctx) {
		if err = c.Decode(&result); err != nil {
			err = errors.AddContext(err, "failed to decode DB data")
			return
		}
		count++
		totalSize += result.Size
		totalBandwidth += skynet.BandwidthDownloadCost(result.Size)
	}
	return count, totalSize, totalBandwidth, nil
}

// userRegistryWriteStats reports the number of registry writes by the user and
// the bandwidth used.
func (db *DB) userRegistryWriteStats(ctx context.Context, userID primitive.ObjectID, monthStart time.Time) (int64, int64, error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userID},
		{"timestamp", bson.D{{"$gt", monthStart}}},
	}}}
	writes, err := db.count(ctx, db.staticRegistryWrites, matchStage)
	if err != nil {
		return 0, 0, errors.AddContext(err, "failed to fetch registry write bandwidth")
	}
	return writes, writes * skynet.CostBandwidthRegistryWrite, nil
}

// userRegistryReadsStats reports the number of registry reads by the user and
// the bandwidth used.
func (db *DB) userRegistryReadStats(ctx context.Context, userID primitive.ObjectID, monthStart time.Time) (int64, int64, error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userID},
		{"timestamp", bson.D{{"$gt", monthStart}}},
	}}}
	reads, err := db.count(ctx, db.staticRegistryReads, matchStage)
	if err != nil {
		return 0, 0, errors.AddContext(err, "failed to fetch registry read bandwidth")
	}
	return reads, reads * skynet.CostBandwidthRegistryRead, nil
}

// monthStart returns the start of the user's subscription month.
// Users get their bandwidth quota reset at the start of the month.
func monthStart(subscribedUntil time.Time) time.Time {
	now := time.Now().UTC()
	// Check how many days are left until the end of the user's subscription
	// month. Then calculate when the last subscription month started. We don't
	// care if the user is no longer subscribed and their sub expired 3 months
	// ago, all we care about here is the day of the month on which that
	// happened because that is the day from which we count their statistics for
	// the month. If they were never subscribed we use Jan 1st 1970 for
	// SubscribedUntil.
	daysDelta := subscribedUntil.Day() - now.Day()
	monthsDelta := 0
	if daysDelta > 0 {
		// The end of sub day is after the current date, so the start of month
		// is in the previous month.
		monthsDelta = -1
	}
	d := now.AddDate(0, monthsDelta, daysDelta)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}
