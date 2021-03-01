package database

import (
	"context"
	"sync"
	"time"

	"github.com/NebulousLabs/skynet-accounts/build"
	"github.com/NebulousLabs/skynet-accounts/skynet"

	"gitlab.com/NebulousLabs/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	// TierReserved reserved
	TierReserved = iota
	// TierFree free
	TierFree
	// TierPremium5 5
	TierPremium5
	// TierPremium20 20
	TierPremium20
	// TierPremium80 80
	TierPremium80
)

type (
	// User represents a Skynet user.
	User struct {
		// ID is a hexadecimal string representation of the MongoDB id assigned
		// to this user object. It is auto-generated by Mongo on insert.
		ID              primitive.ObjectID `bson:"_id,omitempty" json:"-"`
		Sub             string             `bson:"sub" json:"sub"`
		Tier            int                `bson:"tier" json:"tier"`
		SubscribedUntil time.Time          `bson:"subscribed_until" json:"subscribedUntil"`
		StripeId        string             `bson:"stripe_id" json:"stripeCustomerId"`
	}
	// UserStats contains statistical information about the user.
	UserStats struct {
		StorageUsed        int64 `json:"storageUsed"`
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
)

// UserBySub returns the user with the given sub. If `create` is `true` it will
// create the user if it doesn't exist. The sub is the Kratos id of that user.
func (db *DB) UserBySub(ctx context.Context, sub string, create bool) (*User, error) {
	users, err := db.managedUsersByField(ctx, "sub", sub)
	if create && errors.Contains(err, ErrUserNotFound) {
		return db.UserCreate(ctx, sub, TierFree)
	}
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
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
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

// UserByStripeID finds a user by their Stripe customer id.
func (db *DB) UserByStripeID(ctx context.Context, id string) (*User, error) {
	filter := bson.D{{"stripe_id", id}}
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
		build.Critical("more than one user found for stripe customer id", id)
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
	u := &User{
		ID:   primitive.ObjectID{},
		Sub:  sub,
		Tier: tier,
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

// UserStats returns statistical information about the user.
func (db *DB) UserStats(ctx context.Context, user User) (*UserStats, error) {
	return db.userStats(ctx, user)
}

// UserDelete deletes a user by their ID.
func (db *DB) UserDelete(ctx context.Context, u *User) error {
	if u.ID.IsZero() {
		return errors.AddContext(ErrUserNotFound, "user struct not fully initialised")
	}
	filter := bson.D{{"_id", u.ID}}
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
	opts := options.Update().SetUpsert(true)
	_, err := db.staticUsers.UpdateOne(ctx, filter, u, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	return nil
}

// UserSetStripeId changes the user's stripe id in the DB.
func (db *DB) UserSetStripeId(ctx context.Context, u *User, stripeId string) error {
	filter := bson.M{"_id": u.ID}
	update := bson.M{"$set": bson.M{
		"stripe_id": stripeId,
	}}
	opts := options.Update().SetUpsert(true)
	_, err := db.staticUsers.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return errors.AddContext(err, "failed to update")
	}
	return nil
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
		n, size, storage, bw, err := db.userUploadStats(ctx, user.ID, startOfMonth)
		if err != nil {
			regErr("Failed to get user's upload bandwidth used:", err)
			return
		}
		stats.NumUploads = n
		stats.TotalUploadsSize = size
		stats.StorageUsed = storage
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
func (db *DB) userUploadStats(ctx context.Context, id primitive.ObjectID, monthStart time.Time) (count int, totalSize int64, storageUsed int64, totalBandwidth int64, err error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", id},
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
		{"skylink", 0},
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
		storageUsed += skynet.StorageUsed(result.Size)
		totalBandwidth += skynet.BandwidthUploadCost(result.Size)
	}
	return count, totalSize, storageUsed, totalBandwidth, nil
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
func (db *DB) userRegistryWriteStats(ctx context.Context, userId primitive.ObjectID, monthStart time.Time) (int64, int64, error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userId},
		{"timestamp", bson.D{{"$gt", monthStart}}},
	}}}
	writes, err := db.count(ctx, db.staticRegistryWrites, matchStage)
	if err != nil {
		return 0, 0, errors.AddContext(err, "failed to fetch registry write bandwidth")
	}
	return writes, writes * skynet.PriceBandwidthRegistryWrite, nil
}

// userRegistryReadsStats reports the number of registry reads by the user and
// the bandwidth used.
func (db *DB) userRegistryReadStats(ctx context.Context, userId primitive.ObjectID, monthStart time.Time) (int64, int64, error) {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userId},
		{"timestamp", bson.D{{"$gt", monthStart}}},
	}}}
	reads, err := db.count(ctx, db.staticRegistryReads, matchStage)
	if err != nil {
		return 0, 0, errors.AddContext(err, "failed to fetch registry read bandwidth")
	}
	return reads, reads * skynet.PriceBandwidthRegistryRead, nil
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
	d := now.AddDate(0, -1, daysDelta)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}
