// Copyright 2017 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package writers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/matrix-org/dendrite/clientapi/auth/authtypes"
	"github.com/matrix-org/dendrite/clientapi/auth/storage/accounts"
	"github.com/matrix-org/dendrite/clientapi/httputil"
	"github.com/matrix-org/dendrite/clientapi/jsonerror"
	"github.com/matrix-org/dendrite/clientapi/producers"
	"github.com/matrix-org/dendrite/common/config"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
)

// JoinRoomByIDOrAlias implements the "/join/{roomIDOrAlias}" API.
// https://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-join-roomidoralias
func JoinRoomByIDOrAlias(
	req *http.Request,
	device *authtypes.Device,
	roomIDOrAlias string,
	cfg config.Dendrite,
	federation *gomatrixserverlib.FederationClient,
	producer *producers.RoomserverProducer,
	queryAPI api.RoomserverQueryAPI,
	aliasAPI api.RoomserverAliasAPI,
	keyRing gomatrixserverlib.KeyRing,
	accountDB *accounts.Database,
) util.JSONResponse {
	var content map[string]interface{} // must be a JSON object
	if resErr := httputil.UnmarshalJSONRequest(req, &content); resErr != nil {
		return *resErr
	}

	localpart, _, err := gomatrixserverlib.SplitID('@', device.UserID)
	if err != nil {
		return httputil.LogThenError(req, err)
	}

	profile, err := accountDB.GetProfileByLocalpart(localpart)
	if err != nil {
		return httputil.LogThenError(req, err)
	}

	content["membership"] = "join"
	content["displayname"] = profile.DisplayName
	content["avatar_url"] = profile.AvatarURL

	r := joinRoomReq{req, content, device.UserID, cfg, federation, producer, queryAPI, aliasAPI, keyRing}

	if strings.HasPrefix(roomIDOrAlias, "!") {
		return r.joinRoomByID()
	}
	if strings.HasPrefix(roomIDOrAlias, "#") {
		return r.joinRoomByAlias(roomIDOrAlias)
	}
	return util.JSONResponse{
		Code: 400,
		JSON: jsonerror.BadJSON("Invalid first character for room ID or alias"),
	}
}

type joinRoomReq struct {
	req        *http.Request
	content    map[string]interface{}
	userID     string
	cfg        config.Dendrite
	federation *gomatrixserverlib.FederationClient
	producer   *producers.RoomserverProducer
	queryAPI   api.RoomserverQueryAPI
	aliasAPI   api.RoomserverAliasAPI
	keyRing    gomatrixserverlib.KeyRing
}

// joinRoomByID joins a room by room ID
func (r joinRoomReq) joinRoomByID() util.JSONResponse {
	// TODO: Implement joining rooms by ID.
	// A client should only join a room by room ID when it has an invite
	// to the room. If the server is already in the room then we can
	// lookup the invite and process the request as a normal state event.
	// If the server is not in the room the we will need to look up the
	// remote server the invite came from in order to request a join event
	// from that server.
	panic(fmt.Errorf("Joining rooms by ID is not implemented"))
}

// joinRoomByAlias joins a room using a room alias.
func (r joinRoomReq) joinRoomByAlias(roomAlias string) util.JSONResponse {
	_, domain, err := gomatrixserverlib.SplitID('#', roomAlias)
	if err != nil {
		return util.JSONResponse{
			Code: 400,
			JSON: jsonerror.BadJSON("Room alias must be in the form '#localpart:domain'"),
		}
	}
	if domain == r.cfg.Matrix.ServerName {
		queryReq := api.GetAliasRoomIDRequest{Alias: roomAlias}
		var queryRes api.GetAliasRoomIDResponse
		if err = r.aliasAPI.GetAliasRoomID(&queryReq, &queryRes); err != nil {
			return httputil.LogThenError(r.req, err)
		}

		if len(queryRes.RoomID) > 0 {
			return r.joinRoomUsingServers(queryRes.RoomID, []gomatrixserverlib.ServerName{r.cfg.Matrix.ServerName})
		}
		// If the response doesn't contain a non-empty string, return an error
		return util.JSONResponse{
			Code: 404,
			JSON: jsonerror.NotFound("Room alias " + roomAlias + " not found."),
		}
	}
	// If the room isn't local, use federation to join
	return r.joinRoomByRemoteAlias(domain, roomAlias)
}

func (r joinRoomReq) joinRoomByRemoteAlias(
	domain gomatrixserverlib.ServerName, roomAlias string,
) util.JSONResponse {
	resp, err := r.federation.LookupRoomAlias(domain, roomAlias)
	if err != nil {
		switch x := err.(type) {
		case gomatrix.HTTPError:
			if x.Code == 404 {
				return util.JSONResponse{
					Code: 404,
					JSON: jsonerror.NotFound("Room alias not found"),
				}
			}
		}
		return httputil.LogThenError(r.req, err)
	}

	return r.joinRoomUsingServers(resp.RoomID, resp.Servers)
}

func (r joinRoomReq) writeToBuilder(eb *gomatrixserverlib.EventBuilder, roomID string) {
	eb.Type = "m.room.member"
	eb.SetContent(r.content)
	eb.SetUnsigned(struct{}{})
	eb.Sender = r.userID
	eb.StateKey = &r.userID
	eb.RoomID = roomID
	eb.Redacts = ""
}

func (r joinRoomReq) joinRoomUsingServers(
	roomID string, servers []gomatrixserverlib.ServerName,
) util.JSONResponse {
	var eb gomatrixserverlib.EventBuilder
	r.writeToBuilder(&eb, roomID)

	needed, err := gomatrixserverlib.StateNeededForEventBuilder(&eb)
	if err != nil {
		return httputil.LogThenError(r.req, err)
	}

	// Ask the roomserver for information about this room
	queryReq := api.QueryLatestEventsAndStateRequest{
		RoomID:       roomID,
		StateToFetch: needed.Tuples(),
	}
	var queryRes api.QueryLatestEventsAndStateResponse
	if queryErr := r.queryAPI.QueryLatestEventsAndState(&queryReq, &queryRes); queryErr != nil {
		return httputil.LogThenError(r.req, queryErr)
	}

	if queryRes.RoomExists {
		// The room exists in the local database, so we just have to send a join
		// membership event and return the room ID
		// TODO: Check if the user is allowed in the room (has been invited if
		// the room is invite-only)
		eb.Depth = queryRes.Depth
		eb.PrevEvents = queryRes.LatestEvents

		authEvents := gomatrixserverlib.NewAuthEvents(nil)

		for i := range queryRes.StateEvents {
			authEvents.AddEvent(&queryRes.StateEvents[i])
		}

		refs, err := needed.AuthEventReferences(&authEvents)
		if err != nil {
			return httputil.LogThenError(r.req, err)
		}
		eb.AuthEvents = refs

		now := time.Now()
		eventID := fmt.Sprintf("$%s:%s", util.RandomString(16), r.cfg.Matrix.ServerName)
		event, err := eb.Build(
			eventID, now, r.cfg.Matrix.ServerName, r.cfg.Matrix.KeyID, r.cfg.Matrix.PrivateKey,
		)
		if err != nil {
			return httputil.LogThenError(r.req, err)
		}

		if err := r.producer.SendEvents([]gomatrixserverlib.Event{event}, r.cfg.Matrix.ServerName); err != nil {
			return httputil.LogThenError(r.req, err)
		}

		return util.JSONResponse{
			Code: 200,
			JSON: struct {
				RoomID string `json:"room_id"`
			}{roomID},
		}
	}

	if len(servers) == 0 {
		return util.JSONResponse{
			Code: 404,
			JSON: jsonerror.NotFound("No candidate servers found for room"),
		}
	}

	var lastErr error
	for _, server := range servers {
		var response *util.JSONResponse
		response, lastErr = r.joinRoomUsingServer(roomID, server)
		if lastErr != nil {
			// There was a problem talking to one of the servers.
			util.GetLogger(r.req.Context()).WithError(lastErr).WithField("server", server).Warn("Failed to join room using server")
			// Try the next server.
			continue
		}
		return *response
	}

	// Every server we tried to join through resulted in an error.
	// We return the error from the last server.

	// TODO: Generate the correct HTTP status code for all different
	// kinds of errors that could have happened.
	// The possible errors include:
	//   1) We can't connect to the remote servers.
	//   2) None of the servers we could connect to think we are allowed
	//	    to join the room.
	//   3) The remote server returned something invalid.
	//   4) We couldn't fetch the public keys needed to verify the
	//      signatures on the state events.
	//   5) ...
	return httputil.LogThenError(r.req, lastErr)
}

// joinRoomUsingServer tries to join a remote room using a given matrix server.
// If there was a failure communicating with the server or the response from the
// server was invalid this returns an error.
// Otherwise this returns a JSONResponse.
func (r joinRoomReq) joinRoomUsingServer(roomID string, server gomatrixserverlib.ServerName) (*util.JSONResponse, error) {
	respMakeJoin, err := r.federation.MakeJoin(server, roomID, r.userID)
	if err != nil {
		// TODO: Check if the user was not allowed to join the room.
		return nil, err
	}

	// Set all the fields to be what they should be, this should be a no-op
	// but it's possible that the remote server returned us something "odd"
	r.writeToBuilder(&respMakeJoin.JoinEvent, roomID)

	now := time.Now()
	eventID := fmt.Sprintf("$%s:%s", util.RandomString(16), r.cfg.Matrix.ServerName)
	event, err := respMakeJoin.JoinEvent.Build(
		eventID, now, r.cfg.Matrix.ServerName, r.cfg.Matrix.KeyID, r.cfg.Matrix.PrivateKey,
	)
	if err != nil {
		res := httputil.LogThenError(r.req, err)
		return &res, nil
	}

	respSendJoin, err := r.federation.SendJoin(server, event)
	if err != nil {
		return nil, err
	}

	if err = respSendJoin.Check(r.keyRing, event); err != nil {
		return nil, err
	}

	if err = r.producer.SendEventWithState(
		gomatrixserverlib.RespState(respSendJoin), event,
	); err != nil {
		res := httputil.LogThenError(r.req, err)
		return &res, nil
	}

	return &util.JSONResponse{
		Code: 200,
		// TODO: Put the response struct somewhere common.
		JSON: struct {
			RoomID string `json:"room_id"`
		}{roomID},
	}, nil
}
