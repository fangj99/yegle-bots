package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"
	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/delay"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch"
)

// TelegramAPIBase is the API base of telegram API.
const TelegramAPIBase = `https://api.telegram.org/`

// BatchSize is the number of top stories to fetch from Hacker News.
const BatchSize = 30

// NumCommentsThreshold is the threshold for number of comments. Story with less
// than this threshold will not be posted in the channel.
const NumCommentsThreshold = 5

// ScoreThreshold is the threshold for the score. Story with less than this
// threshold will not be posted in the channel.
const ScoreThreshold = 50

// DefaultTimeout is the default URLFetch timeout.
const DefaultTimeout = 9 * time.Minute

// DefaultChatID is the default chat ID.
const DefaultChatID = `@yahnc`

func loge(ctx context.Context, err error) {
	log.Errorf(ctx, "%+v", err)
}

var editMessageFunc = delay.Func("editMessage", func(ctx context.Context, itemID int64, messageID int64) {
	log.Infof(ctx, "editing message: id %d, message id %d", itemID, messageID)
	story := Story{ID: itemID, MessageID: messageID}
	err := story.EditMessage(ctx)
	if err != nil {
		if errors.Cause(err) != ErrIgnoredItem {
			loge(ctx, err)
		}
		return
	}
	key := GetKey(ctx, itemID)
	if _, err := datastore.Put(ctx, key, &story); err != nil {
		loge(ctx, err)
	}
})

var sendMessageFunc = delay.Func("sendMessage", func(ctx context.Context, itemID int64) {
	log.Infof(ctx, "sending message: id %d", itemID)
	story := Story{ID: itemID}
	err := story.SendMessage(ctx)
	if err != nil {
		if errors.Cause(err) != ErrIgnoredItem {
			loge(ctx, err)
		}
		return
	}
	key := GetKey(ctx, itemID)
	if _, err := datastore.Put(ctx, key, &story); err != nil {
		loge(ctx, err)
	}
})

var deleteMessageFunc = delay.Func("deleteMessage", func(ctx context.Context, itemID int64, messageID int64) {
	log.Infof(ctx, "deleting message: id %d, message id %d", itemID, messageID)
	story := Story{ID: itemID, MessageID: messageID}
	if err := story.DeleteMessage(ctx); err != nil {
		loge(ctx, err)
	}
})

func init() {
	http.HandleFunc("/poll", handler)
	http.HandleFunc("/cleanup", cleanUpHandler)
}

// TelegramAPI is a helper function to get the Telegram API endpoint.
func TelegramAPI(method string) string {
	return TelegramAPIBase + os.Getenv("BOT_KEY") + "/" + method
}

// NewsURL is a helper function to get the URL to the story's HackerNews page.
func NewsURL(id int64) string {
	return `https://news.ycombinator.com/item?id=` + strconv.FormatInt(id, 10)
}

// ItemURL is a helper function to get the API of an item.
func ItemURL(id int64) string {
	return fmt.Sprintf(`https://hacker-news.firebaseio.com/v0/item/%d.json`, id)
}

// GetTopStoryURL is a helper function to get the
func GetTopStoryURL() string {
	return fmt.Sprintf(`https://hacker-news.firebaseio.com/v0/topstories.json?orderBy="$key"&limitToFirst=%d`, BatchSize)
}

// GetKey get a datastore key for the given item ID.
func GetKey(ctx context.Context, i int64) *datastore.Key {
	root := datastore.NewKey(ctx, "TopStory", "Root", 0, nil)
	return datastore.NewKey(ctx, "Story", "", i, root)
}

func handler(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)

	topStories, err := getTopStories(ctx, BatchSize)
	if err != nil {
		loge(ctx, err)
		return
	}

	var keys []*datastore.Key

	for _, story := range topStories {
		keys = append(keys, GetKey(ctx, story))
	}

	savedStories := make([]Story, BatchSize, BatchSize)

	err = datastore.GetMulti(ctx, keys, savedStories)
	var wg sync.WaitGroup
	defer wg.Wait()
	if err == nil {
		log.Infof(ctx, "no unknown news")
		wg.Add(len(keys))
		for i, key := range keys {
			go func(id, messageID int64) {
				defer wg.Done()
				editMessageFunc.Call(ctx, id, messageID)
			}(key.IntID(), savedStories[i].MessageID)
		}
		return
	}

	multiErr, ok := err.(appengine.MultiError)

	if !ok {
		log.Debugf(ctx, "%v", errors.Wrap(err, "in func handler() from datastore.GetMulti()"))
		return
	}

	for i, err := range multiErr {
		switch {
		case err == nil:
			wg.Add(1)
			go func(id, messageID int64) {
				defer wg.Done()
				editMessageFunc.Call(ctx, id, messageID)
			}(keys[i].IntID(), savedStories[i].MessageID)
		case err == datastore.ErrNoSuchEntity:
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				sendMessageFunc.Call(ctx, id)
			}(keys[i].IntID())
		default:
			loge(ctx, err)
		}
	}
}

func getTopStories(ctx context.Context, limit int) ([]int64, error) {
	resp, err := myHTTPClient(ctx).Get(GetTopStoryURL())
	if err != nil {
		return nil, errors.Wrap(err, "getTopStories -> http.Client.Get")
	}
	defer resp.Body.Close()

	var ret []int64
	if err := json.NewDecoder(resp.Body).Decode(&ret); err != nil {
		return nil, errors.Wrap(err, "in getTopStories from json.Decoder.Decode()")
	}

	return ret, nil
}

func myHTTPClient(ctx context.Context) *http.Client {
	withTimeout, _ := context.WithTimeout(ctx, DefaultTimeout)
	return urlfetch.Client(withTimeout)
}

func cleanUpHandler(w http.ResponseWriter, r *http.Request) {
	ctx := appengine.NewContext(r)
	var allStories []Story

	now := time.Now()
	oneDayAgo := now.Add(-24 * time.Hour)
	_, err := datastore.NewQuery("Story").Filter("LastSave <=", oneDayAgo).GetAll(ctx, &allStories)
	if err != nil {
		loge(ctx, err)
		return
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	for _, story := range allStories {
		wg.Add(1)
		go func(id, messageID int64) {
			defer wg.Done()
			deleteMessageFunc.Call(ctx, id, messageID)
		}(story.ID, story.MessageID)
	}
}
