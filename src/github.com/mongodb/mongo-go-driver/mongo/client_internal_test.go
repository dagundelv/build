// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package mongo

import (
	"context"
	"os"
	"path"
	"testing"

	"fmt"

	"github.com/mongodb/mongo-go-driver/bson"
	"github.com/mongodb/mongo-go-driver/core/readpref"
	"github.com/mongodb/mongo-go-driver/core/tag"
	"github.com/mongodb/mongo-go-driver/internal/testutil"
	"github.com/stretchr/testify/require"

	"time"

	"github.com/mongodb/mongo-go-driver/core/session"
	"github.com/mongodb/mongo-go-driver/core/uuid"
	"github.com/mongodb/mongo-go-driver/core/writeconcern"
	"github.com/mongodb/mongo-go-driver/mongo/clientopt"
	"github.com/mongodb/mongo-go-driver/mongo/sessionopt"
)

func createTestClient(t *testing.T) *Client {
	id, _ := uuid.New()
	return &Client{
		id:             id,
		topology:       testutil.Topology(t),
		connString:     testutil.ConnString(t),
		readPreference: readpref.Primary(),
		clock:          &session.ClusterClock{},
		registry:       bson.DefaultRegistry,
	}
}

func TestNewClient(t *testing.T) {
	t.Parallel()

	c := createTestClient(t)
	require.NotNil(t, c.topology)
}

func TestClient_Database(t *testing.T) {
	t.Parallel()

	dbName := "foo"

	c := createTestClient(t)
	db := c.Database(dbName)
	require.Equal(t, db.Name(), dbName)
	require.Exactly(t, c, db.Client())
}

func TestClientOptions(t *testing.T) {
	t.Parallel()

	c, err := NewClientWithOptions("mongodb://localhost",
		clientopt.MaxConnIdleTime(200),
		clientopt.ReplicaSet("test"),
		clientopt.LocalThreshold(10),
		clientopt.MaxConnIdleTime(100),
		clientopt.LocalThreshold(20))
	require.NoError(t, err)

	require.Equal(t, time.Duration(20), c.connString.LocalThreshold)
	require.Equal(t, time.Duration(100), c.connString.MaxConnIdleTime)
	require.Equal(t, "test", c.connString.ReplicaSet)
}

func TestClient_TLSConnection(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}

	caFile := os.Getenv("MONGO_GO_DRIVER_CA_FILE")

	if len(caFile) == 0 {
		t.Skip()
	}

	c := createTestClient(t)
	db := c.Database("test")

	result, err := db.RunCommand(context.Background(), bson.NewDocument(bson.EC.Int32("serverStatus", 1)))
	require.NoError(t, err)

	security, err := result.LookupErr("security")
	require.Nil(t, err)

	require.Equal(t, security.Type, bson.TypeEmbeddedDocument)

	_, found := security.Document().LookupErr("SSLServerSubjectName")
	require.Nil(t, found)

	_, found = security.Document().LookupErr("SSLServerHasCertificateAuthority")
	require.Nil(t, found)

}

func TestClient_X509Auth(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}

	caFile := os.Getenv("MONGO_GO_DRIVER_CA_FILE")

	if len(caFile) == 0 || os.Getenv("AUTH") == "auth" {
		t.Skip()
	}

	const user = "C=US,ST=New York,L=New York City,O=MongoDB,OU=other,CN=external"

	c := createTestClient(t)
	db := c.Database("$external")

	// We don't care if the user doesn't already exist.
	_, _ = db.RunCommand(
		context.Background(),
		bson.NewDocument(
			bson.EC.String("dropUser", user),
		),
	)

	_, err := db.RunCommand(
		context.Background(),
		bson.NewDocument(
			bson.EC.String("createUser", user),
			bson.EC.ArrayFromElements("roles",
				bson.VC.DocumentFromElements(
					bson.EC.String("role", "readWrite"),
					bson.EC.String("db", "test"),
				),
			),
		),
	)
	require.NoError(t, err)

	basePath := path.Join("..", "data", "certificates")
	baseConnString := testutil.ConnString(t)
	cs := fmt.Sprintf(
		"%s&sslClientCertificateKeyFile=%s&authMechanism=MONGODB-X509",
		baseConnString.String(),
		path.Join(basePath, "client.pem"),
	)

	authClient, err := NewClient(cs)
	require.NoError(t, err)

	err = authClient.Connect(context.Background())
	require.NoError(t, err)

	db = authClient.Database("test")
	rdr, err := db.RunCommand(
		context.Background(),
		bson.NewDocument(
			bson.EC.Int32("connectionStatus", 1),
		),
	)
	require.NoError(t, err)

	users, err := rdr.LookupErr("authInfo", "authenticatedUsers")
	require.NoError(t, err)

	array := users.Array()
	elems, err := array.Elements()
	require.NoError(t, err)

	for _, v := range elems {
		rdr := v.Value().Document()
		var u struct {
			User string
			DB   string
		}

		if err := bson.Unmarshal(rdr, &u); err != nil {
			continue
		}

		if u.User == user && u.DB == "$external" {
			return
		}
	}

	t.Error("unable to find authenticated user")
}

func TestClient_ListDatabases_noFilter(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}

	dbName := "listDatabases_noFilter"
	c := createTestClient(t)
	db := c.Database(dbName)
	coll := db.Collection("test")
	coll.writeConcern = writeconcern.New(writeconcern.WMajority())
	_, err := coll.InsertOne(
		context.Background(),
		bson.NewDocument(
			bson.EC.Int32("x", 1),
		),
	)
	require.NoError(t, err)

	dbs, err := c.ListDatabases(context.Background(), nil)
	require.NoError(t, err)
	found := false

	for _, db := range dbs.Databases {

		if db.Name == dbName {
			found = true
			break
		}
	}
	require.True(t, found)
}

func TestClient_ListDatabases_filter(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}

	skipIfBelow36(t)

	dbName := "listDatabases_filter"

	c := createTestClient(t)
	db := c.Database(dbName)
	coll := db.Collection("test")
	coll.writeConcern = writeconcern.New(writeconcern.WMajority())
	_, err := coll.InsertOne(
		context.Background(),
		bson.NewDocument(
			bson.EC.Int32("x", 1),
		),
	)
	require.NoError(t, err)

	dbs, err := c.ListDatabases(
		context.Background(),
		bson.NewDocument(
			bson.EC.Regex("name", dbName, ""),
		),
	)

	require.Equal(t, len(dbs.Databases), 1)
	require.Equal(t, dbName, dbs.Databases[0].Name)
}

func TestClient_ListDatabaseNames_noFilter(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}

	dbName := "listDatabasesNames_noFilter"

	c := createTestClient(t)
	db := c.Database(dbName)
	coll := db.Collection("test")

	coll.writeConcern = writeconcern.New(writeconcern.WMajority())
	_, err := coll.InsertOne(
		context.Background(),
		bson.NewDocument(
			bson.EC.Int32("x", 1),
		),
	)
	require.NoError(t, err)

	dbs, err := c.ListDatabaseNames(context.Background(), nil)
	found := false

	for _, name := range dbs {
		if name == dbName {
			found = true
			break
		}
	}
	require.True(t, found)
}

func TestClient_ListDatabaseNames_filter(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}

	skipIfBelow36(t)

	dbName := "listDatabasesNames_filter"

	c := createTestClient(t)
	db := c.Database(dbName)
	coll := db.Collection("test")
	coll.writeConcern = writeconcern.New(writeconcern.WMajority())
	_, err := coll.InsertOne(
		context.Background(),
		bson.NewDocument(
			bson.EC.Int32("x", 1),
		),
	)
	require.NoError(t, err)

	dbs, err := c.ListDatabaseNames(
		context.Background(),
		bson.NewDocument(
			bson.EC.Regex("name", dbName, ""),
		),
	)

	require.NoError(t, err)
	require.Len(t, dbs, 1)
	require.Equal(t, dbName, dbs[0])
}

func TestClient_ReadPreference(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip()
	}
	var tags = []tag.Set{
		{
			tag.Tag{
				Name:  "one",
				Value: "1",
			},
		},
		{
			tag.Tag{
				Name:  "two",
				Value: "2",
			},
		},
	}
	baseConnString := testutil.ConnString(t)
	cs := testutil.AddOptionsToURI(baseConnString.String(), "readpreference=secondary&readPreferenceTags=one:1&readPreferenceTags=two:2&maxStaleness=5")

	c, err := NewClient(cs)
	require.NoError(t, err)
	require.NotNil(t, c)
	require.Equal(t, readpref.SecondaryMode, c.readPreference.Mode())
	require.Equal(t, tags, c.readPreference.TagSets())
	d, flag := c.readPreference.MaxStaleness()
	require.True(t, flag)
	require.Equal(t, time.Duration(5)*time.Second, d)
}

func TestClient_ReadPreferenceAbsent(t *testing.T) {
	t.Parallel()

	cs := testutil.ConnString(t)
	c, err := NewClient(cs.String())
	require.NoError(t, err)
	require.NotNil(t, c)
	require.Equal(t, readpref.PrimaryMode, c.readPreference.Mode())
	require.Empty(t, c.readPreference.TagSets())
	_, flag := c.readPreference.MaxStaleness()
	require.False(t, flag)
}

func TestClient_CausalConsistency(t *testing.T) {
	cs := testutil.ConnString(t)
	c, err := NewClient(cs.String())
	require.NoError(t, err)
	require.NotNil(t, c)

	err = c.Connect(ctx)
	require.NoError(t, err)

	s, err := c.StartSession(sessionopt.CausalConsistency(true))
	sess := s.(*sessionImpl)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.Consistent)
	sess.EndSession(ctx)

	s, err = c.StartSession(sessionopt.CausalConsistency(false))
	sess = s.(*sessionImpl)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.False(t, sess.Consistent)
	sess.EndSession(ctx)

	s, err = c.StartSession()
	sess = s.(*sessionImpl)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.Consistent)
	sess.EndSession(ctx)
}

func TestClient_Ping_DefaultReadPreference(t *testing.T) {
	cs := testutil.ConnString(t)
	c, err := NewClient(cs.String())
	require.NoError(t, err)
	require.NotNil(t, c)

	err = c.Connect(ctx)
	require.NoError(t, err)

	err = c.Ping(ctx, nil)
	require.NoError(t, err)
}

func TestClient_Ping_InvalidHost(t *testing.T) {
	c, err := NewClientWithOptions("mongodb://nohost:27017", clientopt.ServerSelectionTimeout(1*time.Millisecond))
	require.NoError(t, err)
	require.NotNil(t, c)

	err = c.Connect(ctx)
	require.NoError(t, err)

	err = c.Ping(ctx, nil)
	require.NotNil(t, err)
}
