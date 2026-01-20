package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

var tiktokRegex = regexp.MustCompile(`https?://(?:vm|vt|www)\.tiktok\.com/[a-zA-Z0-9/]+`)

const (
	cacheTTL       = 10 * time.Minute
	startTokenPref = "tt_"
	logPrefix      = "[tiktok-bot]"
)

type cacheItem struct {
	url     string
	expires time.Time
}

type linkCache struct {
	mu    sync.Mutex
	items map[string]cacheItem
}

func newLinkCache() *linkCache {
	return &linkCache{items: make(map[string]cacheItem)}
}

func (c *linkCache) set(url string) string {
	token := randomToken(12)
	c.mu.Lock()
	c.items[token] = cacheItem{url: url, expires: time.Now().Add(cacheTTL)}
	c.mu.Unlock()
	return token
}

func (c *linkCache) get(token string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[token]
	if !ok {
		return "", false
	}
	if time.Now().After(item.expires) {
		delete(c.items, token)
		return "", false
	}
	return item.url, true
}

func randomToken(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	opts := []bot.Option{
		bot.WithDefaultHandler(mainRouter),
	}

	token := os.Getenv("TOKEN")
	if token == "" {
		loadEnvFile(".env")
		token = os.Getenv("TOKEN")
	}
	if token == "" {
		panic("empty TOKEN env")
	}

	b, err := bot.New(token, opts...)
	if err != nil {
		panic(err)
	}

	fmt.Println("Бот запущен...")
	b.Start(ctx)
}

var inlineCache = newLinkCache()

func logTikTok(format string, args ...interface{}) {
	log.Printf("%s %s", logPrefix, fmt.Sprintf(format, args...))
}

func mainRouter(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.InlineQuery != nil {
		log.Printf("incoming update type=inline_query id=%s query=%q", update.InlineQuery.ID, update.InlineQuery.Query)
		handlerInline(ctx, b, update)
		return
	}
	if update.Message != nil {
		log.Printf("incoming update type=message chat_id=%d text=%q", update.Message.Chat.ID, update.Message.Text)
		handlerMessage(ctx, b, update)
	}
}

func handlerInline(ctx context.Context, b *bot.Bot, update *models.Update) {
	inlineID := update.InlineQuery.ID
	query := update.InlineQuery.Query
	log.Printf("inline query received id=%s query=%q", inlineID, query)

	link := tiktokRegex.FindString(query)
	log.Printf("inline query parsed link id=%s link=%q", inlineID, link)
	if link == "" {
		return
	}
	logTikTok("handlerInline link=%s", link)

	rs, err := fetchTikTok(link)
	hasImages := rs != nil && len(rs.Data.Images) > 0
	hasPlay := rs != nil && rs.Data.Play != ""
	imageCount := 0
	if rs != nil {
		imageCount = len(rs.Data.Images)
	}
	if err != nil || (rs.Data.Play == "" && len(rs.Data.Images) == 0) {
		status := "success"
		if err != nil {
			status = "error"
		}
		if rs == nil {
			logTikTok("handlerInline fetch status=%s link=%s err=%v", status, link, err)
		} else {
			logTikTok(
				"handlerInline fetch status=%s link=%s has_images=%t has_play=%t image_count=%d err=%v",
				status,
				link,
				len(rs.Data.Images) > 0,
				rs.Data.Play != "",
				len(rs.Data.Images),
				err,
			)
		}
		return
	}
	logTikTok(
		"handlerInline fetch status=success link=%s has_images=%t has_play=%t image_count=%d",
		link,
		len(rs.Data.Images) > 0,
		rs.Data.Play != "",
		len(rs.Data.Images),
	)

	if hasImages {
		botUsername := os.Getenv("BOT_USERNAME")
		if botUsername == "" {
			log.Printf("inline query bot username empty id=%s link=%q", inlineID, link)
			return
		}

		token := inlineCache.set(link)
		deepLink := fmt.Sprintf("https://t.me/%s?start=%s%s", botUsername, startTokenPref, token)

		results := []models.InlineQueryResult{
			&models.InlineQueryResultArticle{
				ID:          "1",
				Title:       "Слайдшоу TikTok",
				Description: "Откройте чат с ботом, чтобы получить все фото",
				InputMessageContent: &models.InputTextMessageContent{
					MessageText: fmt.Sprintf("Нажмите кнопку ниже, чтобы скачать слайдшоу. [src](%s)", link),
					ParseMode:   models.ParseModeMarkdown,
				},
				ReplyMarkup: &models.InlineKeyboardMarkup{
					InlineKeyboard: [][]models.InlineKeyboardButton{
						{
							{Text: "Открыть в боте", URL: deepLink},
						},
					},
				},
			},
		}

		b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
			InlineQueryID: update.InlineQuery.ID,
			Results:       results,
			CacheTime:     300,
		})
		return
	}

	results := []models.InlineQueryResult{
		&models.InlineQueryResultVideo{
			ID:           "1",
			VideoURL:     rs.Data.Play,
			MimeType:     "video/mp4",
			ThumbnailURL: rs.Data.Cover,
			Title:        rs.Data.Title,
			Caption:      fmt.Sprintf("[src](%s)", link),
			ParseMode:    models.ParseModeMarkdown,
		},
	}

	b.AnswerInlineQuery(ctx, &bot.AnswerInlineQueryParams{
		InlineQueryID: update.InlineQuery.ID,
		Results:       results,
		CacheTime:     300,
	})
}

func handlerMessage(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message.Text == "" {
		return
	}

	if handleStartPayload(ctx, b, update) {
		return
	}

	link := tiktokRegex.FindString(update.Message.Text)
	if link == "" {
		return
	}
	logTikTok("handlerMessage link=%s", link)

	rs, err := sendTikTok(ctx, b, update.Message.Chat.ID, link)
	if err != nil {
		logTikTok("handlerMessage fetch status=error link=%s err=%v", link, err)
		return
	}
	logTikTok(
		"handlerMessage fetch status=success link=%s has_images=%t has_play=%t image_count=%d",
		link,
		len(rs.Data.Images) > 0,
		rs.Data.Play != "",
		len(rs.Data.Images),
	)
}

func handleStartPayload(ctx context.Context, b *bot.Bot, update *models.Update) bool {
	parts := strings.Fields(update.Message.Text)
	if len(parts) == 0 || parts[0] != "/start" {
		return false
	}
	if len(parts) < 2 {
		return false
	}

	payload := strings.TrimSpace(parts[1])
	if !strings.HasPrefix(payload, startTokenPref) {
		return false
	}

	token := strings.TrimPrefix(payload, startTokenPref)
	link, ok := inlineCache.get(token)
	if !ok {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Ссылка устарела. Сначала отправьте ссылку заново через inline режим.",
		})
		return true
	}

	sendTikTok(ctx, b, update.Message.Chat.ID, link)
	return true
}

func sendTikTok(ctx context.Context, b *bot.Bot, chatID int64, link string) (*response, error) {
	rs, err := fetchTikTok(link)
	if err != nil {
		logTikTok("sendTikTok status=error link=%s err=%v", link, err)
		return nil, err
	}
	logTikTok(
		"sendTikTok status=success link=%s has_images=%t has_play=%t image_count=%d",
		link,
		len(rs.Data.Images) > 0,
		rs.Data.Play != "",
		len(rs.Data.Images),
	)

	if hasImages {
		caption := fmt.Sprintf("[src](%s)", link)
		sendPhotoGroups(ctx, b, chatID, rs.Data.Images, caption)
		return rs, nil
	}

	if rs.Data.Play != "" {
		sizeText := "unknown"
		if size, err := fetchContentLength(rs.Data.Play); err == nil {
			sizeText = fmt.Sprintf("%d bytes", size)
		}
		fmt.Printf("TikTok video found, sending: url=%s size=%s\n", rs.Data.Play, sizeText)

		b.SendVideo(ctx, &bot.SendVideoParams{
			ChatID: chatID,
			Video:  &models.InputFileString{Data: rs.Data.Play},
		})
	}
	return rs, nil
}

func sendPhotoGroups(ctx context.Context, b *bot.Bot, chatID int64, imageURLs []string, caption string) {
	const groupSize = 10

	for i := 0; i < len(imageURLs); i += groupSize {
		end := i + groupSize
		if end > len(imageURLs) {
			end = len(imageURLs)
		}
		chunk := imageURLs[i:end]

		media := make([]models.InputMedia, 0, len(chunk))
		for idx, url := range chunk {
			item := &models.InputMediaPhoto{Media: url}
			if idx == 0 && caption != "" {
				item.Caption = caption
				item.ParseMode = models.ParseModeMarkdown
			}
			media = append(media, item)
		}

		b.SendMediaGroup(ctx, &bot.SendMediaGroupParams{
			ChatID: chatID,
			Media:  media,
		})
	}
}

func fetchTikTok(url string) (*response, error) {
	resp, err := http.Get(fmt.Sprintf("https://www.tikwm.com/api/?url=%s", url))
	if err != nil {
		logTikTok("fetchTikTok status=error link=%s err=%v", url, err)
		return nil, err
	}
	defer resp.Body.Close()
	logTikTok("fetchTikTok http_status=%d link=%s", resp.StatusCode, url)

	var rs response
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		logTikTok("fetchTikTok json_decode_error link=%s err=%v", url, err)
		return nil, err
	}
	logTikTok(
		"fetchTikTok status=success link=%s has_images=%t has_play=%t image_count=%d",
		url,
		len(rs.Data.Images) > 0,
		rs.Data.Play != "",
		len(rs.Data.Images),
	)
	return &rs, nil
}

func fetchContentLength(url string) (int64, error) {
	resp, err := http.Head(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.ContentLength <= 0 {
		return 0, fmt.Errorf("unknown content length")
	}
	return resp.ContentLength, nil
}

func makeVideo(imageURLs []string, audioURL string, duration int) ([]byte, error) {
	if duration <= 0 {
		duration = 10
	}

	var wg sync.WaitGroup
	imgBufs := make([][]byte, len(imageURLs))
	var audBuf []byte

	wg.Add(len(imageURLs) + 1)

	go func() {
		defer wg.Done()
		audBuf, _ = downloadToMem(audioURL)
	}()

	for i, url := range imageURLs {
		go func(idx int, u string) {
			defer wg.Done()
			imgBufs[idx], _ = downloadToMem(u)
		}(i, url)
	}
	wg.Wait()

	if audBuf == nil {
		return nil, fmt.Errorf("failed to download audio")
	}

	imgReader, imgWriter, _ := os.Pipe()
	audReader, audWriter, _ := os.Pipe()

	frameRate := float64(len(imageURLs)) / float64(duration)
	vf := "scale=480:854:force_original_aspect_ratio=decrease,pad=480:854:(ow-iw)/2:(oh-ih)/2,setsar=1"

	cmd := exec.Command("ffmpeg", "-y",
		"-framerate", fmt.Sprintf("%f", frameRate),
		"-f", "image2pipe", "-i", "pipe:3",
		"-i", "pipe:4",
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "stillimage",
		"-c:a", "aac", "-b:a", "96k",
		"-pix_fmt", "yuv420p", "-vf", vf,
		"-shortest", "-fflags", "+genpts",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)

	cmd.ExtraFiles = []*os.File{imgReader, audReader}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go func() {
		defer imgWriter.Close()
		for _, b := range imgBufs {
			if b != nil {
				imgWriter.Write(b)
			}
		}
	}()

	go func() {
		defer audWriter.Close()
		audWriter.Write(audBuf)
	}()

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v, stderr: %s", err, errBuf.String())
	}

	return outBuf.Bytes(), nil
}

func downloadToMem(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func loadEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), "\"")
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}
}

type response struct {
	Data struct {
		Title     string   `json:"title"`
		Cover     string   `json:"cover"`
		Play      string   `json:"play"`
		Music     string   `json:"music"`
		Images    []string `json:"images"`
		MusicInfo struct {
			Duration int `json:"duration"`
		} `json:"music_info"`
	} `json:"data"`
}
