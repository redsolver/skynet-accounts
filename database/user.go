package database

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NebulousLabs/skynet-accounts/build"
	"github.com/NebulousLabs/skynet-accounts/jwt"
	"github.com/NebulousLabs/skynet-accounts/skynet"

	"gitlab.com/NebulousLabs/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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
	// False is a helper for when we need to pass a *bool to MongoDB.
	False = false
	// UserLimits defines the limits for each tier.
	// RegistryDelay delay is in ms.
	UserLimits = map[int]TierLimits{
		TierAnonymous: {
			UploadBandwidth:   5 * mbpsToBytesPerSecond,
			DownloadBandwidth: 20 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.GiB,
			MaxNumberUploads:  0,
			RegistryDelay:     250,
			Storage:           0,
		},
		TierFree: {
			UploadBandwidth:   10 * mbpsToBytesPerSecond,
			DownloadBandwidth: 40 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.GiB,
			MaxNumberUploads:  0.1 * filesAllowedPerTB,
			RegistryDelay:     125,
			Storage:           100 * skynet.GiB,
		},
		TierPremium5: {
			UploadBandwidth:   20 * mbpsToBytesPerSecond,
			DownloadBandwidth: 80 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.GiB,
			MaxNumberUploads:  1 * filesAllowedPerTB,
			RegistryDelay:     0,
			Storage:           1 * skynet.TiB,
		},
		TierPremium20: {
			UploadBandwidth:   40 * mbpsToBytesPerSecond,
			DownloadBandwidth: 160 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.GiB,
			MaxNumberUploads:  4 * filesAllowedPerTB,
			RegistryDelay:     0,
			Storage:           4 * skynet.TiB,
		},
		TierPremium80: {
			UploadBandwidth:   80 * mbpsToBytesPerSecond,
			DownloadBandwidth: 320 * mbpsToBytesPerSecond,
			MaxUploadSize:     1 * skynet.GiB,
			MaxNumberUploads:  20 * filesAllowedPerTB,
			RegistryDelay:     0,
			Storage:           20 * skynet.TiB,
		},
	}
)

type (
	// User represents a Skynet user.
	User struct {
		// ID is auto-generated by Mongo on insert. We will usually use it in
		// its ID.Hex() form.
		ID                            primitive.ObjectID `bson:"_id,omitempty" json:"-"`
		FirstName                     string             `bson:"first_name" json:"firstName"`
		LastName                      string             `bson:"last_name" json:"lastName"`
		Email                         string             `bson:"email" json:"email"`
		Sub                           string             `bson:"sub" json:"sub"`
		Tier                          int                `bson:"tier" json:"tier"`
		SubscribedUntil               time.Time          `bson:"subscribed_until" json:"subscribedUntil"`
		SubscriptionStatus            string             `bson:"subscription_status" json:"subscriptionStatus"`
		SubscriptionCancelAt          time.Time          `bson:"subscription_cancel_at" json:"subscriptionCancelAt"`
		SubscriptionCancelAtPeriodEnd bool               `bson:"subscription_cancel_at_period_end" json:"subscriptionCancelAtPeriodEnd"`
		StripeId                      string             `bson:"stripe_id" json:"stripeCustomerId"`
		QuotaExceeded                 bool               `bson:"quota_exceeded" json:"quotaExceeded"`
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
		UploadBandwidth   int   `json:"upload"`        // bytes per second
		DownloadBandwidth int   `json:"download"`      // bytes per second
		MaxUploadSize     int64 `json:"maxUploadSize"` // the max size of a single upload in bytes
		MaxNumberUploads  int   `json:"-"`
		RegistryDelay     int   `json:"registry"` // ms delay
		Storage           int64 `json:"-"`
	}
)

// UserBySub returns the user with the given sub. If `create` is `true` it will
// create the user if it doesn't exist. The sub is the Kratos id of that user.
func (db *DB) UserBySub(ctx context.Context, sub string, create bool) (*User, error) {
	users, err := db.managedUsersByField(ctx, "sub", sub)
	if create && errors.Contains(err, ErrUserNotFound) {
		var u *User
		u, err = db.UserCreate(ctx, sub, TierFree)
		// If we're successful or hit any error, other than a duplicate key we
		// want to just return. Hitting a duplicate key error means we ran into
		// a race condition and we can easily recover from that.
		if err == nil || !strings.Contains(err.Error(), "E11000 duplicate key error collection") {
			return u, err
		}
		// Recover from the race condition by fetching the existing user from
		// the DB.
		users, err = db.managedUsersByField(ctx, "sub", sub)
	}
	if err != nil {
		return nil, err
	}
	return users[0], nil
}

// UserByID finds a user by their ID.
func (db *DB) UserByID(ctx context.Context, userID primitive.ObjectID) (*User, error) {
	filter := bson.D{{"_id", userID}}
	c, err := db.staticUsers.Find(ctx, filter)
	if err != nil {
		return nil, errors.AddContext(err, "failed to Find")
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()
	// Get the first result.
	if ok := c.Next(ctx); !ok {
		return nil, ErrUserNotFound
	}
	// Ensure there are no more results.
	if ok := c.Next(ctx); ok {
		build.Critical("more than one user found for userID", userID)
	}
	var u User
	err = c.Decode(&u)
	if err != nil {
		return nil, errors.AddContext(err, "failed to parse value from DB")
	}
	return &u, nil
}

// UserByStripeID finds a user by their Stripe customer id.
func (db *DB) UserByStripeID(ctx context.Context, stripeID string) (*User, error) {
	filter := bson.D{{"stripe_id", stripeID}}
	c, err := db.staticUsers.Find(ctx, filter)
	if err != nil {
		return nil, errors.AddContext(err, "failed to Find")
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()
	// Get the first result.
	if ok := c.Next(ctx); !ok {
		return nil, ErrUserNotFound
	}
	// Ensure there are no more results.
	if ok := c.Next(ctx); ok {
		build.Critical(fmt.Sprintf("more than one user found for stripe customer id '%s'", stripeID))
	}
	var u User
	err = c.Decode(&u)
	if err != nil {
		return nil, errors.AddContext(err, "failed to parse value from DB")
	}
	return &u, nil
}

// UserCreate creates a new user in the DB.
func (db *DB) UserCreate(ctx context.Context, sub string, tier int) (*User, error) {
	// Check for an existing user with this sub.
	users, err := db.managedUsersByField(ctx, "sub", sub)
	if err != nil && !errors.Contains(err, ErrUserNotFound) {
		return nil, errors.AddContext(err, "failed to query DB")
	}
	if len(users) > 0 {
		return nil, ErrUserAlreadyExists
	}
	fName, lName, email, err := jwt.UserDetailsFromJWT(ctx)
	if err != nil {
		// Log the error but don't do anything differently.
		db.staticLogger.Debugf("We failed to extract the expected user infotmation from the JWT token. Error: %s", err.Error())
	}
	u := &User{
		ID:        primitive.ObjectID{},
		FirstName: fName,
		LastName:  lName,
		Email:     email,
		Sub:       sub,
		Tier:      tier,
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
func (db *DB) UserDelete(ctx context.Context, user *User) error {
	if user.ID.IsZero() {
		return errors.AddContext(ErrUserNotFound, "user struct not fully initialised")
	}
	filter := bson.D{{"_id", user.ID}}
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
func (db *DB) UserSave(ctx context.Context, user *User) error {
	filter := bson.M{"_id": user.ID}
	opts := &options.ReplaceOptions{
		Upsert: &True,
	}
	_, err := db.staticUsers.ReplaceOne(ctx, filter, user, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	return nil
}

// UserSetStripeId changes the user's stripe id in the DB.
func (db *DB) UserSetStripeId(ctx context.Context, user *User, stripeID string) error {
	filter := bson.M{"_id": user.ID}
	update := bson.M{"$set": bson.M{
		"stripe_id": stripeID,
	}}
	opts := options.Update().SetUpsert(true)
	_, err := db.staticUsers.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	return nil
}

// UserSetTier sets the user's tier to the given value.
func (db *DB) UserSetTier(ctx context.Context, user *User, tier int) error {
	if tier <= TierAnonymous || tier >= TierMaxReserved {
		return errors.New("invalid tier value")
	}
	filter := bson.M{"_id": user.ID}
	update := bson.M{"$set": bson.M{
		"tier": tier,
	}}
	opts := options.Update().SetUpsert(true)
	_, err := db.staticUsers.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	user.Tier = tier
	return nil
}

// UserStats returns statistical information about the user.
func (db *DB) UserStats(ctx context.Context, user User) (*UserStats, error) {
	return db.userStats(ctx, user)
}

// managedUsersByField finds all users that have a given field value.
// The calling method is responsible for the validation of the value.
func (db *DB) managedUsersByField(ctx context.Context, fieldName, fieldValue string) ([]*User, error) {
	filter := bson.D{{fieldName, fieldValue}}
	c, err := db.staticUsers.Find(ctx, filter)
	if err != nil {
		return nil, errors.AddContext(err, "failed to find user")
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
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

// userStats reports statistical information about the user.
func (db *DB) userStats(ctx context.Context, user User) (*UserStats, error) {
	stats := UserStats{}
	var errs []error
	var errsMux sync.Mutex
	regErr := func(msg string, e error) {
		db.staticLogger.Info(msg, e)
		errsMux.Lock()
		errs = append(errs, e)
		errsMux.Unlock()
	}
	startOfMonth := monthStart(user.SubscribedUntil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, size, rawStorage, bw, err := db.userUploadStats(ctx, user.ID, startOfMonth)
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

// userUploadStats reports on the user's uploads - count, total size and total
// bandwidth used. It uses the total size of the uploaded skyfiles as basis.
func (db *DB) userUploadStats(ctx context.Context, userID primitive.ObjectID, monthStart time.Time) (count int, totalSize int64, rawStorageUsed int64, totalBandwidth int64, err error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userID},
		{"timestamp", bson.D{{"$gt", monthStart}}},
	}}}
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
		{"timestamp", 0},
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

	processedSkylinks := make(map[string]bool)
	for c.Next(ctx) {
		// We need this struct, so we can safely decode both int32 and int64.
		result := struct {
			Size     int64  `bson:"size"`
			Skylink  string `bson:"skylink"`
			Unpinned bool   `bson:"unpinned"`
		}{}
		if err = c.Decode(&result); err != nil {
			err = errors.AddContext(err, "failed to decode DB data")
			return
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
func (db *DB) userDownloadStats(ctx context.Context, userID primitive.ObjectID, monthStart time.Time) (count int, totalSize int64, totalBandwidth int64, err error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userID},
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

	for c.Next(ctx) {
		// We need this struct, so we can safely decode both int32 and int64.
		result := struct {
			Size int64 `bson:"size"`
		}{}
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
