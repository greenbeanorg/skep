package localapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/userapi"
)

// The users-read and users-write messages are package userapi's (step
// 8c-1), which skep-api imports without the rest of this package.
type (
	UsersAnswer       = userapi.UsersAnswer
	UsersWriteRequest = userapi.UsersWriteRequest
	UsersWriteResult  = userapi.UsersWriteResult
	UserInfo          = userapi.UserInfo
)

// MaxUsersWait is the longest wait of users-read.
const MaxUsersWait = userapi.MaxUsersWait

func usersReadQuery(r *http.Request) (uint64, time.Duration, bool) {
	q := r.URL.Query()
	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		after = n
	}
	var wait time.Duration
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || time.Duration(n)*time.Second > userapi.MaxUsersWait {
			return 0, 0, false
		}
		wait = time.Duration(n) * time.Second
	}
	return after, wait, true
}
