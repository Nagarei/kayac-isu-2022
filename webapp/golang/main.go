package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/oklog/ulid/v2"
	"github.com/srinathgs/mysqlstore"
	"golang.org/x/crypto/bcrypt"

	_ "net/http/pprof"

	"github.com/felixge/fgprof"
)

const (
	publicPath        = "./public"
	sessionCookieName = "listen80_session_golang"
	anonUserAccount   = "__"
)

var (
	db           *sqlx.DB
	sessionStore sessions.Store
	tr           = &renderer{templates: template.Must(template.ParseGlob("views/*.html"))}
	// for use ULID
	entropy = ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0)
)

func getEnv(key string, defaultValue string) string {
	val := os.Getenv(key)
	if val != "" {
		return val
	}
	return defaultValue
}

func connectDB() (*sqlx.DB, error) {
	config := mysql.NewConfig()
	config.Net = "tcp"
	config.Addr = getEnv("ISUCON_DB_HOST", "127.0.0.1") + ":" + getEnv("ISUCON_DB_PORT", "3306")
	config.User = getEnv("ISUCON_DB_USER", "isucon")
	config.Passwd = getEnv("ISUCON_DB_PASSWORD", "isucon")
	config.DBName = getEnv("ISUCON_DB_NAME", "isucon_listen80")
	config.ParseTime = true
	config.InterpolateParams = true

	dsn := config.FormatDSN()
	return sqlx.Open("mysql", dsn)
}

type renderer struct {
	templates *template.Template
}

func (t *renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}

func cacheControllPrivate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderCacheControl, "private")
		return next(c)
	}
}

func main() {

	http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
	go func() {
		http.ListenAndServe(":6060", nil)
	}()

	e := echo.New()
	e.Debug = false
	e.Logger.SetLevel(log.ERROR)

	//e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(cacheControllPrivate)

	e.Renderer = tr
	e.Static("/assets", publicPath+"/assets")
	e.GET("/mypage", authRequiredPageHandler)
	e.GET("/playlist/:ulid/edit", authRequiredPageHandler)
	e.GET("/", authOptionalPageHandler)
	e.GET("/playlist/:ulid", authOptionalPageHandler)
	e.GET("/signup", authPageHandler)
	e.GET("/login", authPageHandler)

	e.POST("/api/signup", apiSignupHandler)
	e.POST("/api/login", apiLoginHandler)
	e.POST("/api/logout", apiLogoutHandler)
	e.GET("/api/recent_playlists", apiRecentPlaylistsHandler)
	e.GET("/api/popular_playlists", apiPopularPlaylistsHandler)
	e.GET("/api/playlists", apiPlaylistsHandler)
	e.GET("/api/playlist/:playlistUlid", apiPlaylistHandler)
	e.POST("/api/playlist/add", apiPlaylistAddHandler)
	e.POST("/api/playlist/:playlistUlid/update", apiPlaylistUpdateHandler)
	e.POST("/api/playlist/:playlistUlid/delete", apiPlaylistDeleteHandler)
	e.POST("/api/playlist/:playlistUlid/favorite", apiPlaylistFavoriteHandler)
	e.POST("/api/admin/user/ban", apiAdminUserBanHandler)

	e.POST("/initialize", initializeHandler)

	var err error
	db, err = connectDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	db.SetMaxOpenConns(700)
	defer db.Close()

	sessionStore, err = mysqlstore.NewMySQLStoreFromConnection(db.DB, "sessions_golang", "/", 86400, []byte("powawa"))
	if err != nil {
		e.Logger.Fatalf("failed to initialize session store: %v", err)
		return
	}

	port := getEnv("SERVER_APP_PORT", "3000")
	e.Logger.Infof("starting listen80 server on : %s ...", port)
	serverPort := fmt.Sprintf(":%s", port)
	e.Logger.Fatal(e.Start(serverPort))
}

func getSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.Get(r, sessionCookieName)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func newSession(r *http.Request) (*sessions.Session, error) {
	session, err := sessionStore.New(r, sessionCookieName)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func errorResponse(c echo.Context, code int, message string) error {
	c.Logger().Debugf("error: status=%d, message=%s", code, message)

	body := BasicResponse{
		Result: false,
		Status: code,
		Error:  &message,
	}
	if code == 401 {
		sess, err := getSession(c.Request())
		if err != nil {
			return fmt.Errorf("error getSession at errorResponse: %w", err)
		}
		sess.Options.MaxAge = -1
		if err := sess.Save(c.Request(), c.Response()); err != nil {
			return fmt.Errorf("error Save to session at errorResponse: %w", err)
		}
	}
	if err := c.JSON(code, body); err != nil {
		return fmt.Errorf("error returns JSON at errorResponse: %w", err)
	}
	return nil
}

func validateSession(c echo.Context) (*UserRow, bool, error) {
	sess, err := getSession(c.Request())
	if err != nil {
		return nil, false, fmt.Errorf("error getSession: %w", err)
	}
	_account, ok := sess.Values["user_account"]
	if !ok {
		return nil, false, nil
	}
	account := _account.(string)
	var user UserRow
	err = db.GetContext(
		c.Request().Context(),
		&user,
		"SELECT * FROM user where `account` = ?",
		account,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("error Get UserRow from db: %w", err)
	}
	if user.IsBan {
		return nil, false, nil
	}

	return &user, true, nil
}

func generatePasswordHash(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), 11)
	if err != nil {
		return "", fmt.Errorf("error bcrypt.GenerateFromPassword: %w", err)
	}
	return string(hashed), nil
}

func comparePasswordHash(newPassword, passwordHash string) (bool, error) {
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(newPassword)); err != nil {
		if err == bcrypt.ErrMismatchedHashAndPassword {
			return false, nil
		}
		return false, fmt.Errorf("error bcrypt.CompateHashAndPassword: %w", err)
	}
	return true, nil
}

// ?????????????????????

type TemplateParams struct {
	LoggedIn    bool
	Params      map[string]string
	DisplayName string
	UserAccount string
}

var authRequiredPages = map[string]string{
	"/mypage":              "mypage.html",
	"/playlist/:ulid/edit": "playlist_edit.html",
}

func authRequiredPageHandler(c echo.Context) error {
	user, ok, err := validateSession(c)
	if err != nil {
		return fmt.Errorf("error %s at authRequired: %w", c.Path(), err)
	}
	if !ok || user == nil {
		c.Redirect(http.StatusFound, "/")
		return nil
	}
	page := authRequiredPages[c.Path()]

	return c.Render(http.StatusOK, page, &TemplateParams{
		LoggedIn: true,
		Params: map[string]string{
			"ulid": c.Param("ulid"),
		},
		DisplayName: user.DisplayName,
		UserAccount: user.Account,
	})
}

var authOptionalPages = map[string]string{
	"/":               "top.html",
	"/playlist/:ulid": "playlist.html",
}

func authOptionalPageHandler(c echo.Context) error {
	user, ok, err := validateSession(c)
	if err != nil {
		return fmt.Errorf("error %s at authRequired: %w", c.Path(), err)
	}
	if user != nil && user.IsBan {
		return errorResponse(c, 401, "failed to fetch user (no such user)")
	}

	var displayName, account string
	if user != nil {
		displayName = user.DisplayName
		account = user.Account
	}
	page := authOptionalPages[c.Path()]
	return c.Render(http.StatusOK, page, &TemplateParams{
		LoggedIn: ok,
		Params: map[string]string{
			"ulid": c.Param("ulid"),
		},
		DisplayName: displayName,
		UserAccount: account,
	})
}

var authPages = map[string]string{
	"/signup": "signup.html",
	"/login":  "login.html",
}

func authPageHandler(c echo.Context) error {
	page := authPages[c.Path()]
	return c.Render(http.StatusOK, page, &TemplateParams{
		LoggedIn: false,
	})
}

// DB???????????????????????????????????????????????????

func getPlaylistByULID(ctx context.Context, db connOrTx, playlistULID string) (*PlaylistRow, error) {
	var row PlaylistRow
	if err := db.GetContext(ctx, &row, "SELECT * FROM playlist WHERE `ulid` = ?", playlistULID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("error Get playlist by ulid=%s: %w", playlistULID, err)
	}
	return &row, nil
}

func getPlaylistByID(ctx context.Context, db connOrTx, playlistID int) (*PlaylistRow, error) {
	var row PlaylistRow
	if err := db.GetContext(ctx, &row, "SELECT * FROM playlist WHERE `id` = ?", playlistID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("error Get playlist by id=%d: %w", playlistID, err)
	}
	return &row, nil
}

type connOrTx interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func getSongByULID(ctx context.Context, db connOrTx, songULID string) (*SongRow, error) {
	var row SongRow
	if err := db.GetContext(ctx, &row, "SELECT * FROM song WHERE `ulid` = ?", songULID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("error Get song by ulid=%s: %w", songULID, err)
	}
	return &row, nil
}

func isFavoritedBy(ctx context.Context, db connOrTx, userAccount string, playlistID int) (bool, error) {
	var count int
	if err := db.GetContext(
		ctx,
		&count,
		"SELECT COUNT(*) AS cnt FROM playlist_favorite where favorite_user_account = ? AND playlist_id = ?",
		userAccount, playlistID,
	); err != nil {
		return false, fmt.Errorf(
			"error Get count of playlist_favorite by favorite_user_account=%s, playlist_id=%d: %w",
			userAccount, playlistID, err,
		)
	}
	return count > 0, nil
}

func getFavoritesCountByPlaylistID(ctx context.Context, db connOrTx, playlistID int) (int, error) {
	var count int
	if err := db.GetContext(
		ctx,
		&count,
		"SELECT count FROM favorite_count where playlist_id = ?",
		playlistID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"error Get count of favorite_count by playlist_id=%d: %w",
			playlistID, err,
		)
	}
	return count, nil
}

func getSongsCountByPlaylistID(ctx context.Context, db connOrTx, playlistID int) (int, error) {
	var count int
	if err := db.GetContext(
		ctx,
		&count,
		"SELECT COUNT(*) AS cnt FROM playlist_song where playlist_id = ?",
		playlistID,
	); err != nil {
		return 0, fmt.Errorf(
			"error Get count of playlist_song by playlist_id=%d: %w",
			playlistID, err,
		)
	}
	return count, nil
}

func getRecentPlaylistSummaries(ctx context.Context, db connOrTx, userAccount string) ([]Playlist, error) {
	playlists := make([]Playlist, 0, 100)
	if err := db.SelectContext(
		ctx,
		&playlists,
		" SELECT pl.id as id, pl.ulid as ulid, pl.name as name, pl.is_public as is_public, pl.created_at as created_at, pl.updated_at as updated_at, display_name, account"+
			" FROM playlist as pl INNER JOIN user ON user.`account` = pl.`user_account`"+
			" where pl.is_public = true AND user.is_ban = false"+
			" ORDER BY pl.created_at DESC LIMIT 100",
	); err != nil {
		return nil, fmt.Errorf(
			"error Select playlist by is_public=true: %w",
			err,
		)
	}
	if len(playlists) == 0 {
		return nil, nil
	}

	for index, playlist := range playlists {
		// user, err := getUserByAccount(ctx, db, playlist.UserAccount)
		// if err != nil {
		// 	return nil, fmt.Errorf("error getUserByAccount: %w", err)
		// }
		// if user == nil || user.IsBan {
		// 	continue
		// }

		songCount, err := getSongsCountByPlaylistID(ctx, db, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error getSongsCountByPlaylistID: %w", err)
		}
		favoriteCount, err := getFavoritesCountByPlaylistID(ctx, db, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error getFavoritesCountByPlaylistID: %w", err)
		}

		var isFavorited bool
		if userAccount != anonUserAccount {
			var err error
			isFavorited, err = isFavoritedBy(ctx, db, userAccount, playlist.ID)
			if err != nil {
				return nil, fmt.Errorf("error isFavoritedBy: %w", err)
			}
		}

		// playlists = append(playlists, Playlist{
		// 	ULID:            playlist.ULID,
		// 	Name:            playlist.Name,
		// 	UserDisplayName: user.DisplayName,
		// 	UserAccount:     user.Account,
		// 	SongCount:       songCount,
		// 	FavoriteCount:   favoriteCount,
		// 	IsFavorited:     isFavorited,
		// 	IsPublic:        playlist.IsPublic,
		// 	CreatedAt:       playlist.CreatedAt,
		// 	UpdatedAt:       playlist.UpdatedAt,
		// })
		playlists[index].SongCount = songCount
		playlists[index].FavoriteCount = favoriteCount
		playlists[index].IsFavorited = isFavorited
	}
	return playlists, nil
}

func getPopularPlaylistSummaries(ctx context.Context, db connOrTx, userAccount string) ([]Playlist, error) {
	playlists := make([]Playlist, 0, 100)
	if err := db.SelectContext(
		ctx,
		&playlists,
		`SELECT
		pl.id as id, pl.ulid as ulid, pl.name as name, pl.is_public as is_public, pl.created_at as created_at, pl.updated_at as updated_at,
		display_name, account, count AS favorite_count
		FROM favorite_count INNER JOIN playlist as pl ON pl.id = favorite_count.playlist_id
		INNER JOIN user ON user.account = pl.user_account
		WHERE favorite_count.count > 0 AND pl.is_public = true AND user.is_ban = false
		ORDER BY count DESC LIMIT 100`,
	); err != nil {
		return nil, fmt.Errorf(
			"error Select favorite_count: %w",
			err,
		)
	}

	if len(playlists) == 0 {
		return nil, nil
	}
	for index, playlist := range playlists {

		songCount, err := getSongsCountByPlaylistID(ctx, db, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error getSongsCountByPlaylistID: %w", err)
		}

		var isFavorited bool
		if userAccount != anonUserAccount {
			// ????????????????????????fav?????????
			var err error
			isFavorited, err = isFavoritedBy(ctx, db, userAccount, playlist.ID)
			if err != nil {
				return nil, fmt.Errorf("error isFavoritedBy: %w", err)
			}
		}

		playlists[index].SongCount = songCount
		playlists[index].IsFavorited = isFavorited
	}
	return playlists, nil
}

func getCreatedPlaylistSummariesByUserAccount(ctx context.Context, db connOrTx, userAccount string) ([]Playlist, error) {
	var playlists []PlaylistRow
	if err := db.SelectContext(
		ctx,
		&playlists,
		"SELECT * FROM playlist where user_account = ? ORDER BY created_at DESC LIMIT 100",
		userAccount,
	); err != nil {
		return nil, fmt.Errorf(
			"error Select playlist by user_account=%s: %w",
			userAccount, err,
		)
	}
	if len(playlists) == 0 {
		return nil, nil
	}

	user, err := getUserByAccount(ctx, db, userAccount)
	if err != nil {
		return nil, fmt.Errorf("error getUserByAccount: %w", err)
	}
	if user == nil || user.IsBan {
		return nil, nil
	}

	results := make([]Playlist, 0, len(playlists))
	for _, row := range playlists {
		songCount, err := getSongsCountByPlaylistID(ctx, db, row.ID)
		if err != nil {
			return nil, fmt.Errorf("error getSongsCountByPlaylistID: %w", err)
		}
		favoriteCount, err := getFavoritesCountByPlaylistID(ctx, db, row.ID)
		if err != nil {
			return nil, fmt.Errorf("error getFavoritesCountByPlaylistID: err=%w", err)
		}
		isFavorited, err := isFavoritedBy(ctx, db, userAccount, row.ID)
		if err != nil {
			return nil, fmt.Errorf("error isFavoritedBy: %w", err)
		}
		results = append(results, Playlist{
			ULID:            row.ULID,
			Name:            row.Name,
			UserDisplayName: user.DisplayName,
			UserAccount:     user.Account,
			SongCount:       songCount,
			FavoriteCount:   favoriteCount,
			IsFavorited:     isFavorited,
			IsPublic:        row.IsPublic,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
	}

	return results, nil
}

func getFavoritedPlaylistSummariesByUserAccount(ctx context.Context, db connOrTx, userAccount string) ([]Playlist, error) {
	var playlistFavorites []PlaylistFavoriteRow
	if err := db.SelectContext(
		ctx,
		&playlistFavorites,
		"SELECT * FROM playlist_favorite where favorite_user_account = ? ORDER BY created_at DESC",
		userAccount,
	); err != nil {
		return nil, fmt.Errorf(
			"error Select playlist_favorite by user_account=%s: %w",
			userAccount, err,
		)
	}

	playlists := make([]Playlist, 0, 100)
	for _, fav := range playlistFavorites {
		playlist, err := getPlaylistByID(ctx, db, fav.PlaylistID)
		if err != nil {
			return nil, fmt.Errorf("error getPlaylistByID: %w", err)
		}
		// ????????????????????????
		if playlist == nil || !playlist.IsPublic {
			continue
		}
		user, err := getUserByAccount(ctx, db, playlist.UserAccount)
		if err != nil {
			return nil, fmt.Errorf("error getUserByAccount: %w", err)
		}

		// ???????????????????????????ban??????????????????????????????
		if user == nil || user.IsBan {
			return nil, nil
		}

		songCount, err := getSongsCountByPlaylistID(ctx, db, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error getSongsCountByPlaylistID: %w", err)
		}
		favoriteCount, err := getFavoritesCountByPlaylistID(ctx, db, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error getFavoritesCountByPlaylistID: err=%w", err)
		}
		isFavorited, err := isFavoritedBy(ctx, db, userAccount, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error isFavoritedBy: %w", err)
		}
		playlists = append(playlists, Playlist{
			ULID:            playlist.ULID,
			Name:            playlist.Name,
			UserDisplayName: user.DisplayName,
			UserAccount:     user.Account,
			SongCount:       songCount,
			FavoriteCount:   favoriteCount,
			IsFavorited:     isFavorited,
			IsPublic:        playlist.IsPublic,
			CreatedAt:       playlist.CreatedAt,
			UpdatedAt:       playlist.UpdatedAt,
		})
		if len(playlists) >= 100 {
			break
		}
	}

	return playlists, nil
}

func getPlaylistDetailByPlaylistULID(ctx context.Context, db connOrTx, playlistULID string, viewerUserAccount *string) (*PlaylistDetail, error) {
	playlist, err := getPlaylistByULID(ctx, db, playlistULID)
	if err != nil {
		return nil, fmt.Errorf("error getPlaylistByULID: %w", err)
	}
	if playlist == nil {
		return nil, nil
	}

	user, err := getUserByAccount(ctx, db, playlist.UserAccount)
	if err != nil {
		return nil, fmt.Errorf("error getUserByAccount: %w", err)
	}
	if user == nil || user.IsBan {
		return nil, nil
	}

	favoriteCount, err := getFavoritesCountByPlaylistID(ctx, db, playlist.ID)
	if err != nil {
		return nil, fmt.Errorf("error getFavoriteCountByPlaylistID: %w", err)
	}
	var isFavorited bool
	if viewerUserAccount != nil {
		var err error
		isFavorited, err = isFavoritedBy(ctx, db, *viewerUserAccount, playlist.ID)
		if err != nil {
			return nil, fmt.Errorf("error isFavoritedBy: %w", err)
		}
	}

	// var resPlaylistSongs []PlaylistSongRow
	// if err := db.SelectContext(
	// 	ctx,
	// 	&resPlaylistSongs,
	// 	"SELECT * FROM playlist_song WHERE playlist_id = ?",
	// 	playlist.ID,
	// ); err != nil {
	// 	return nil, fmt.Errorf(
	// 		"error Select playlist_song by playlist_id=%d: %w",
	// 		playlist.ID, err,
	// 	)
	// }

	// songs := make([]Song, 0, len(resPlaylistSongs))
	// for _, row := range resPlaylistSongs {
	// 	var song SongRow
	// 	if err := db.GetContext(
	// 		ctx,
	// 		&song,
	// 		"SELECT * FROM song WHERE id = ?",
	// 		row.SongID,
	// 	); err != nil {
	// 		return nil, fmt.Errorf("error Get song by id=%d: %w", row.SongID, err)
	// 	}

	// 	var artist ArtistRow
	// 	if err := db.GetContext(
	// 		ctx,
	// 		&artist,
	// 		"SELECT * FROM artist WHERE id = ?",
	// 		song.ArtistID,
	// 	); err != nil {
	// 		return nil, fmt.Errorf("error Get artist by id=%d: %w", song.ArtistID, err)
	// 	}

	// 	songs = append(songs, Song{
	// 		ULID:        song.ULID,
	// 		Title:       song.Title,
	// 		Artist:      artist.Name,
	// 		Album:       song.Album,
	// 		TrackNumber: song.TrackNumber,
	// 		IsPublic:    song.IsPublic,
	// 	})
	// }

	var songs []Song
	if err := db.SelectContext(
		ctx,
		&songs,
		`SELECT song.ulid as ulid, song.title as title, artist.name as artist_name,
			song.album as album, song.track_number as track_number, song.is_public as is_public FROM playlist_song
		JOIN song ON playlist_song.song_id = song.id
		JOIN artist ON song.artist_id = artist.id
		WHERE playlist_id = ?`,
		playlist.ID,
	); err != nil {
		return nil, fmt.Errorf(
			"error Select playlist_song by playlist_id=%d: %w",
			playlist.ID, err,
		)
	}

	return &PlaylistDetail{
		Playlist: &Playlist{
			ULID:            playlist.ULID,
			Name:            playlist.Name,
			UserDisplayName: user.DisplayName,
			UserAccount:     user.Account,
			SongCount:       len(songs),
			FavoriteCount:   favoriteCount,
			IsFavorited:     isFavorited,
			IsPublic:        playlist.IsPublic,
			CreatedAt:       playlist.CreatedAt,
			UpdatedAt:       playlist.UpdatedAt,
		},
		Songs: songs,
	}, nil
}

func getPlaylistFavoritesByPlaylistIDAndUserAccount(ctx context.Context, db connOrTx, playlistID int, favoriteUserAccount string) (*PlaylistFavoriteRow, error) {
	var result []PlaylistFavoriteRow
	if err := db.SelectContext(
		ctx,
		&result,
		"SELECT * FROM playlist_favorite WHERE `playlist_id` = ? AND `favorite_user_account` = ?",
		playlistID,
		favoriteUserAccount,
	); err != nil {
		return nil, fmt.Errorf(
			"error Select playlist_favorite by playlist_id=%d, favorite_user_account=%s: %w",
			playlistID, favoriteUserAccount, err,
		)
	}
	if len(result) == 0 {
		return nil, nil
	}
	return &result[0], nil
}

func getUserByAccount(ctx context.Context, db connOrTx, account string) (*UserRow, error) {
	var result UserRow
	if err := db.GetContext(
		ctx,
		&result,
		"SELECT * FROM user WHERE `account` = ?",
		account,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"error Get user by account=%s: %w",
			account, err,
		)
	}
	return &result, nil
}

func insertPlaylistSong(ctx context.Context, db connOrTx, playlistID, sortOrder, songID int) error {
	if _, err := db.ExecContext(
		ctx,
		"INSERT INTO playlist_song (`playlist_id`, `sort_order`, `song_id`) VALUES (?, ?, ?)",
		playlistID, sortOrder, songID,
	); err != nil {
		return fmt.Errorf(
			"error Insert playlist_song by playlist_id=%d, sort_order=%d, song_id=%d: %w",
			playlistID, sortOrder, songID, err,
		)
	}
	return nil
}

func insertPlaylistFavorite(ctx context.Context, tx *sqlx.Tx, playlistID int, favoriteUserAccount string, createdAt time.Time) error {
	if _, err := tx.ExecContext(
		ctx,
		"INSERT INTO playlist_favorite (`playlist_id`, `favorite_user_account`, `created_at`) VALUES (?, ?, ?)",
		playlistID, favoriteUserAccount, createdAt,
	); err != nil {
		return fmt.Errorf(
			"error Insert playlist_favorite by playlist_id=%d, favorite_user_account=%s, created_at=%s: %w",
			playlistID, favoriteUserAccount, createdAt, err,
		)
	}
	if _, err := tx.ExecContext(
		ctx,
		"INSERT INTO favorite_count (`playlist_id`, `count`) VALUES (?, 1) ON DUPLICATE KEY UPDATE count=count+1",
		playlistID,
	); err != nil {
		return fmt.Errorf(
			"error Insert favorite_count by playlist_id=%d: %w",
			playlistID, err,
		)
	}
	return nil
}

// POST /api/signup

func apiSignupHandler(c echo.Context) error {
	var signupRequest SignupRequest
	if err := c.Bind(&signupRequest); err != nil {
		c.Logger().Errorf("error Bind request to SignupRequest: %s", err)
		return errorResponse(c, 500, "failed to signup")
	}
	userAccount := signupRequest.UserAccount
	password := signupRequest.Password
	displayName := signupRequest.DisplayName

	// validation
	if userAccount == "" || len(userAccount) < 4 || 191 < len(userAccount) {
		return errorResponse(c, 400, "bad user_account")
	}
	if matched, _ := regexp.MatchString(`[^a-zA-Z0-9\-_]`, userAccount); matched {
		return errorResponse(c, 400, "bad user_account")
	}
	if password == "" || len(password) < 8 || 64 < len(password) {
		return errorResponse(c, 400, "bad password")
	}
	if matched, _ := regexp.MatchString(`[^a-zA-Z0-9\-_]`, password); matched {
		return errorResponse(c, 400, "bad password")
	}
	if displayName == "" || utf8.RuneCountInString(displayName) < 2 || 24 < utf8.RuneCountInString(displayName) {
		return errorResponse(c, 400, "bad display_name")
	}

	// password hash?????????
	passwordHash, err := generatePasswordHash(password)
	if err != nil {
		c.Logger().Errorf("error generatePasswordHash: %s", err)
		return errorResponse(c, 500, "failed to signup")
	}

	// default value
	isBan := false
	signupTimestamp := time.Now()

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "failed to signup")
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx,
		"INSERT INTO user (`account`, `display_name`, `password_hash`, `is_ban`, `created_at`, `last_logined_at`) VALUES (?, ?, ?, ?, ?, ?)",
		userAccount, displayName, passwordHash, isBan, signupTimestamp, signupTimestamp,
	); err != nil {
		// handling a "Duplicate entry"
		if merr, ok := err.(*mysql.MySQLError); ok && merr.Number == 1062 {
			return errorResponse(c, 400, "account already exist")
		}
		return fmt.Errorf(
			"error Insert user by user_account=%s, display_name=%s, password_hash=%s, is_ban=%t, created_at=%s, updated_at=%s: %w",

			userAccount, displayName, passwordHash, isBan, signupTimestamp, signupTimestamp, err,
		)
	}

	sess, err := newSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error newSession: %s", err)
		return errorResponse(c, 500, "failed to signup")
	}
	sess.Values["user_account"] = userAccount
	if err := sess.Save(c.Request(), c.Response()); err != nil {
		c.Logger().Errorf("error Save to session: %s", err)
		return errorResponse(c, 500, "failed to signup")
	}

	body := BasicResponse{
		Result: true,
		Status: 200,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "failed to signup")
	}

	return nil
}

// POST /api/login

func apiLoginHandler(c echo.Context) error {
	var loginRequest LoginRequest
	if err := c.Bind(&loginRequest); err != nil {
		c.Logger().Errorf("error Bind request to LoginRequest: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}
	userAccount := loginRequest.UserAccount
	password := loginRequest.Password

	// validation
	if userAccount == "" || len(userAccount) < 4 || 191 < len(userAccount) {
		return errorResponse(c, 400, "bad user_account")
	}
	if matched, _ := regexp.MatchString(`[^a-zA-Z0-9\-_]`, userAccount); matched {
		return errorResponse(c, 400, "bad user_account")
	}
	if password == "" || len(password) < 8 || 64 < len(password) {
		return errorResponse(c, 400, "bad password")
	}
	if matched, _ := regexp.MatchString(`[^a-zA-Z0-9\-_]`, password); matched {
		return errorResponse(c, 400, "bad password")
	}

	// password check
	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}
	defer conn.Close()

	user, err := getUserByAccount(ctx, conn, userAccount)
	if err != nil {
		c.Logger().Errorf("error getUserByAccount: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}
	if user == nil || user.IsBan {
		// ????????????????????????ban???????????????
		return errorResponse(c, 401, "failed to login (no such user)")
	}

	matched, err := comparePasswordHash(password, user.PasswordHash)
	if err != nil {
		c.Logger().Errorf("error comparePasswordHash: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}
	if !matched {
		// wrong password
		return errorResponse(c, 401, "failed to login (wrong password)")
	}

	now := time.Now()
	if _, err := conn.ExecContext(
		ctx,
		"UPDATE user SET last_logined_at = ? WHERE account = ?",
		now, user.Account,
	); err != nil {
		c.Logger().Errorf("error Update user by last_logined_at=%s, account=%s: %s", now, user.Account, err)
		return errorResponse(c, 500, "failed to login (server error)")
	}

	sess, err := newSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error newSession: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}
	sess.Values["user_account"] = userAccount
	if err := sess.Save(c.Request(), c.Response()); err != nil {
		c.Logger().Errorf("error Save to session: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}

	body := BasicResponse{
		Result: true,
		Status: 200,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "failed to login (server error)")
	}

	return nil
}

// POST /api/logout

func apiLogoutHandler(c echo.Context) error {
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "failed to logout (server error)")
	}
	sess.Options.MaxAge = -1
	if err := sess.Save(c.Request(), c.Response()); err != nil {
		c.Logger().Errorf("error Save session:  %s", err)
		return errorResponse(c, 500, "failed to logout (server error)")
	}

	body := BasicResponse{
		Result: true,
		Status: 200,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "failed to logout (server error)")
	}

	return nil
}

// GET /api/recent_playlists

func apiRecentPlaylistsHandler(c echo.Context) error {
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	userAccount := anonUserAccount
	_account, ok := sess.Values["user_account"]
	if ok {
		userAccount = _account.(string)
	}

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	playlists, err := getRecentPlaylistSummaries(ctx, conn, userAccount)
	if err != nil {
		c.Logger().Errorf("error getRecentPlaylistSummaries: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	body := GetRecentPlaylistsResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		Playlists: playlists,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// GET /api/popular_playlists

func apiPopularPlaylistsHandler(c echo.Context) error {
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	userAccount := anonUserAccount
	_account, ok := sess.Values["user_account"]
	if ok {
		userAccount = _account.(string)
	}

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	// ??????????????????????????????????????????fav????????????????????????????????????
	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		c.Logger().Errorf("error conn.BeginTxx: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	playlists, err := getPopularPlaylistSummaries(ctx, tx, userAccount)
	if err != nil {
		tx.Rollback()
		c.Logger().Errorf("error getPopularPlaylistSummaries: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if err := tx.Commit(); err != nil {
		c.Logger().Errorf("error tx.Commit: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	body := GetRecentPlaylistsResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		Playlists: playlists,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// GET /api/playlists

func apiPlaylistsHandler(c echo.Context) error {
	_, valid, err := validateSession(c)
	if err != nil {
		c.Logger().Errorf("error validateSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if !valid {
		return errorResponse(c, 401, "login required")
	}
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	_account := sess.Values["user_account"]
	userAccount := _account.(string)

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	createdPlaylists, err := getCreatedPlaylistSummariesByUserAccount(ctx, conn, userAccount)
	if err != nil {
		c.Logger().Errorf("error getCreatedPlaylistSummariesByUserAccount: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if createdPlaylists == nil {
		createdPlaylists = []Playlist{}
	}
	favoritedPlaylists, err := getFavoritedPlaylistSummariesByUserAccount(ctx, conn, userAccount)
	if err != nil {
		c.Logger().Errorf("error getFavoritedPlaylistSummariesByUserAccount: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	body := GetPlaylistsResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		CreatedPlaylists:   createdPlaylists,
		FavoritedPlaylists: favoritedPlaylists,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// GET /api/playlist/{:playlistUlid}

func apiPlaylistHandler(c echo.Context) error {
	// ?????????????????????
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	userAccount := anonUserAccount
	_account, ok := sess.Values["user_account"]
	if ok {
		userAccount = _account.(string)
	}
	playlistULID := c.Param("playlistUlid")

	// validation
	if playlistULID == "" {
		return errorResponse(c, 400, "bad playlist ulid")
	}
	if matched, _ := regexp.MatchString("[^a-zA-Z0-9]", playlistULID); matched {
		return errorResponse(c, 400, "bad playlist ulid")
	}

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	playlist, err := getPlaylistByULID(ctx, conn, playlistULID)
	if err != nil {
		c.Logger().Errorf("error getPlaylistByULID:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlist == nil {
		return errorResponse(c, 404, "playlist not found")
	}

	// ?????????????????????????????????private????????????????????????????????????
	if playlist.UserAccount != userAccount && !playlist.IsPublic {
		return errorResponse(c, 404, "playlist not found")
	}

	playlistDetails, err := getPlaylistDetailByPlaylistULID(ctx, conn, playlist.ULID, &userAccount)
	if err != nil {
		c.Logger().Errorf("error getPlaylistDetailByPlaylistULID:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlistDetails == nil {
		return errorResponse(c, 404, "playlist not found")
	}

	body := SinglePlaylistResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		Playlist: *playlistDetails,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// POST /api/playlist/add

func apiPlaylistAddHandler(c echo.Context) error {
	_, valid, err := validateSession(c)
	if err != nil {
		c.Logger().Errorf("error validateSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if !valid {
		return errorResponse(c, 401, "login required")
	}

	var addPlaylistRequest AddPlaylistRequest
	if err := c.Bind(&addPlaylistRequest); err != nil {
		c.Logger().Errorf("error Bind request to AddPlaylistRequest: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	name := addPlaylistRequest.Name
	if name == "" || utf8.RuneCountInString(name) < 2 || 191 < utf8.RuneCountInString(name) {
		return errorResponse(c, 400, "invalid name")
	}

	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	_account := sess.Values["user_account"]
	userAccount := _account.(string)
	createTimestamp := time.Now()
	playlistULID, err := ulid.New(ulid.Timestamp(createTimestamp), entropy)
	if err != nil {
		c.Logger().Errorf("error ulid.New: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx,
		"INSERT INTO playlist (`ulid`, `name`, `user_account`, `is_public`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?)",
		playlistULID.String(), name, userAccount, false, createTimestamp, createTimestamp, // ?????????????????????
	); err != nil {
		c.Logger().Errorf(
			"error Insert playlist by ulid=%s, name=%s, user_account=%s, is_public=%t, created_at=%s, updated_at=%s: %s",
			playlistULID, name, userAccount, false, createTimestamp, createTimestamp,
		)
		return errorResponse(c, 500, "internal server error")
	}

	body := AddPlaylistResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		PlaylistULID: playlistULID.String(),
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// POST /api/playlist/update

func apiPlaylistUpdateHandler(c echo.Context) error {
	_, valid, err := validateSession(c)
	if err != nil {
		c.Logger().Errorf("error validateSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if !valid {
		return errorResponse(c, 401, "login required")
	}
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	_account := sess.Values["user_account"]
	userAccount := _account.(string)

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	playlistULID := c.Param("playlistUlid")
	playlist, err := getPlaylistByULID(ctx, conn, playlistULID)
	if err != nil {
		c.Logger().Errorf("error getPlaylistByULID: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlist == nil {
		return errorResponse(c, 404, "playlist not found")
	}
	if playlist.UserAccount != userAccount {
		// ????????????????????????URI???????????????????????????????????????404?????????
		return errorResponse(c, 404, "playlist not found")
	}

	var updatePlaylistRequest UpdatePlaylistRequest
	if err := c.Bind(&updatePlaylistRequest); err != nil {
		c.Logger().Errorf("error Bind request to UpdatePlaylistRequest: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	name := updatePlaylistRequest.Name
	songULIDs := updatePlaylistRequest.SongULIDs
	isPublic := updatePlaylistRequest.IsPublic
	// validation
	if matched, _ := regexp.MatchString("[^a-zA-Z0-9]", playlistULID); matched {
		return errorResponse(c, 404, "bad playlist ulid")
	}
	// ????????????????????????????????????
	if name == nil || *name == "" || songULIDs == nil {
		return errorResponse(c, 400, "name, song_ulids and is_public is required")
	}
	// name???2????????????191????????????
	if utf8.RuneCountInString(*name) < 2 || 191 < utf8.RuneCountInString(*name) {
		return errorResponse(c, 400, "invalid name")
	}
	// ???????????????80???
	if 80 < len(songULIDs) {
		return errorResponse(c, 400, "invalid song_ulids")
	}
	// ?????????????????????????????????
	songULIDsSet := make(map[string]struct{}, len(songULIDs))
	for _, songULID := range songULIDs {
		songULIDsSet[songULID] = struct{}{}
	}
	if len(songULIDsSet) != len(songULIDs) {
		return errorResponse(c, 400, "invalid song_ulids")
	}

	updatedTimestamp := time.Now()

	plSongs := []PlaylistSongRow{}
	for i, songULID := range songULIDs {
		song, err := getSongByULID(ctx, db, songULID)
		if err != nil {
			c.Logger().Errorf("error getSongByULID: %s", err)
			return errorResponse(c, 500, "internal server error")
		}
		if song == nil {
			return errorResponse(c, 400, fmt.Sprintf("song not found. ulid: %s", songULID))
		}
		plSongs = append(plSongs, PlaylistSongRow{playlist.ID, i + 1, song.ID})
	}

	tx, err := conn.BeginTxx(ctx, nil)
	if err != nil {
		c.Logger().Errorf("error conn.BeginTxx: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	// name, is_public?????????
	if _, err := tx.ExecContext(
		ctx,
		"UPDATE playlist SET name = ?, is_public = ?, `updated_at` = ? WHERE `ulid` = ?",
		name, isPublic, updatedTimestamp, playlist.ULID,
	); err != nil {
		tx.Rollback()
		c.Logger().Errorf(
			"error Update playlist by name=%s, is_public=%t, updated_at=%s, ulid=%s: %s",
			name, isPublic, updatedTimestamp, playlist.ULID, err,
		)
		return errorResponse(c, 500, "internal server error")
	}

	// songs???????????????????????????????????????
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM playlist_song WHERE playlist_id = ?",
		playlist.ID,
	); err != nil {
		tx.Rollback()
		c.Logger().Errorf(
			"error Delete playlist_song by id=%d: %s",
			playlist.ID, err,
		)
		return errorResponse(c, 500, "internal server error")
	}
	if _, err := tx.NamedExecContext(
		ctx,
		"INSERT INTO playlist_song (`playlist_id`, `sort_order`, `song_id`) VALUES (:playlist_id, :sort_order, :song_id)",
		plSongs,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("error Insert playlist_song: %w", err)
	}

	if err := tx.Commit(); err != nil {
		c.Logger().Errorf("error tx.Commit: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	playlistDetails, err := getPlaylistDetailByPlaylistULID(ctx, conn, playlist.ULID, &userAccount)
	if err != nil {
		c.Logger().Errorf("error getPlaylistDetailByPlaylistULID: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlistDetails == nil {
		return errorResponse(c, 500, "error occured: getPlaylistDetailByPlaylistULID")
	}

	body := SinglePlaylistResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		Playlist: *playlistDetails,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// POST /api/playlist/delete

func apiPlaylistDeleteHandler(c echo.Context) error {
	_, valid, err := validateSession(c)
	if err != nil {
		c.Logger().Errorf("error validateSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if !valid {
		return errorResponse(c, 401, "login required")
	}
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	_account := sess.Values["user_account"]
	userAccount := _account.(string)

	playlistULID := c.Param("playlistUlid")
	// validation
	if playlistULID == "" {
		return errorResponse(c, 404, "bad playlist ulid")
	}
	if matched, _ := regexp.MatchString("[^a-zA-Z0-9]", playlistULID); matched {
		return errorResponse(c, 404, "bad playlist ulid")
	}

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	playlist, err := getPlaylistByULID(ctx, conn, playlistULID)
	if err != nil {
		c.Logger().Errorf("error getPlaylistByULID: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlist == nil {
		return errorResponse(c, 400, "playlist not found")
	}

	if playlist.UserAccount != userAccount {
		return errorResponse(c, 400, "do not delete other users playlist")
	}

	if _, err := conn.ExecContext(
		ctx,
		"DELETE FROM playlist WHERE `ulid` = ?",
		playlist.ULID,
	); err != nil {
		c.Logger().Errorf("error Delete playlist by ulid=%s: %s", playlist.ULID, err)
		return errorResponse(c, 500, "internal server error")
	}
	if _, err := conn.ExecContext(
		ctx,
		"DELETE FROM playlist_song WHERE playlist_id = ?",
		playlist.ID,
	); err != nil {
		c.Logger().Errorf("error Delete playlist_song by id=%s: %s", playlist.ID, err)
		return errorResponse(c, 500, "internal server error")
	}

	tx, err := conn.BeginTxx(ctx, nil)
	favorite_ok := false
	defer func() {
		if !favorite_ok {
			tx.Rollback()
		}
	}()
	if err != nil {
		c.Logger().Errorf("error conn.BeginTxx: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM playlist_favorite WHERE playlist_id = ?",
		playlist.ID,
	); err != nil {
		c.Logger().Errorf("error Delete playlist_favorite by id=%s: %s", playlist.ID, err)
		return errorResponse(c, 500, "internal server error")
	}
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM favorite_count WHERE playlist_id = ?",
		playlist.ID,
	); err != nil {
		c.Logger().Errorf("error DELETE favorite_count by id=%s: %s", playlist.ID, err)
		return errorResponse(c, 500, "internal server error")
	}
	favorite_ok = true
	if err := tx.Commit(); err != nil {
		c.Logger().Errorf("error tx.Commit: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	body := BasicResponse{
		Result: true,
		Status: 200,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// POST /api/playlist/:playlistUlid/favorite

func apiPlaylistFavoriteHandler(c echo.Context) error {
	user, ok, err := validateSession(c)
	if err != nil {
		c.Logger().Errorf("error validateSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if !ok || user == nil {
		return errorResponse(c, 401, "login required")
	}
	sess, err := getSession(c.Request())
	if err != nil {
		c.Logger().Errorf("error getSession:  %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	_account := sess.Values["user_account"]
	userAccount := _account.(string)

	playlistULID := c.Param("playlistUlid")
	var favoritePlaylistRequest FavoritePlaylistRequest
	if err := c.Bind(&favoritePlaylistRequest); err != nil {
		c.Logger().Errorf("error Bind to FavoritePlaylistRequest: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	isFavorited := favoritePlaylistRequest.IsFavorited
	if playlistULID == "" {
		return errorResponse(c, 404, "bad playlist ulid")
	}
	if matched, _ := regexp.MatchString("[^a-zA-Z0-9]", playlistULID); matched {
		return errorResponse(c, 404, "bad playlist ulid")
	}

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	playlist, err := getPlaylistByULID(ctx, conn, playlistULID)
	if err != nil {
		c.Logger().Errorf("error getPlaylistByULID: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlist == nil {
		return errorResponse(c, 404, "playlist not found")
	}
	// ??????????????????????????????????????????????????????????????????ban???????????????????????????????????????private?????????not found
	if playlist.UserAccount != user.Account {
		if user.IsBan || !playlist.IsPublic {
			return errorResponse(c, 404, "playlist not found")
		}
	}

	if isFavorited {
		// insert
		createdTimestamp := time.Now()
		playlistFavorite, err := getPlaylistFavoritesByPlaylistIDAndUserAccount(
			ctx, db, playlist.ID, userAccount,
		)
		if err != nil {
			c.Logger().Errorf("error getPlaylistFavoritesByPlaylistIDAndUserAccount: %s", err)
			return errorResponse(c, 500, "internal server error")
		}
		if playlistFavorite == nil {
			tx, err := conn.BeginTxx(ctx, nil)
			if err != nil {
				c.Logger().Errorf("error conn.BeginTxx: %s", err)
				return errorResponse(c, 500, "internal server error")
			}
			if err := insertPlaylistFavorite(ctx, tx, playlist.ID, userAccount, createdTimestamp); err != nil {
				tx.Rollback()
				c.Logger().Errorf("error insertPlaylistFavorite: %s", err)
				return errorResponse(c, 500, "internal server error")
			}
			if err := tx.Commit(); err != nil {
				c.Logger().Errorf("error tx.Commit: %s", err)
				return errorResponse(c, 500, "internal server error")
			}
		}
	} else {
		tx, err := conn.BeginTxx(ctx, nil)
		if err != nil {
			c.Logger().Errorf("error conn.BeginTxx: %s", err)
			return errorResponse(c, 500, "internal server error")
		}
		favorite_ok := false
		defer func() {
			if !favorite_ok {
				tx.Rollback()
			}
		}()
		// delete
		if _, err := tx.ExecContext(
			ctx,
			"DELETE FROM playlist_favorite WHERE `playlist_id` = ? AND `favorite_user_account` = ?",
			playlist.ID, userAccount,
		); err != nil {
			c.Logger().Errorf(
				"error Delete playlist_favorite by playlist_id=%d, favorite_user_account=%s: %s",
				playlist.ID, userAccount, err,
			)
			return errorResponse(c, 500, "internal server error")
		}
		if _, err := tx.ExecContext(
			ctx,
			"UPDATE favorite_count SET count=count-1 WHERE playlist_id = ?",
			playlist.ID,
		); err != nil {
			c.Logger().Errorf("error UPDATE favorite_count by id=%s: %s", playlist.ID, err)
			return errorResponse(c, 500, "internal server error")
		}
		favorite_ok = true
		if err := tx.Commit(); err != nil {
			c.Logger().Errorf("error tx.Commit: %s", err)
			return errorResponse(c, 500, "internal server error")
		}
	}

	playlistDetail, err := getPlaylistDetailByPlaylistULID(ctx, conn, playlist.ULID, &userAccount)
	if err != nil {
		c.Logger().Errorf("error getPlaylistDetailByPlaylistULID: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if playlistDetail == nil {
		return errorResponse(c, 404, "failed to fetch playlist detail")
	}

	body := SinglePlaylistResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		Playlist: *playlistDetail,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

// POST /api/admin/user/ban

func apiAdminUserBanHandler(c echo.Context) error {
	user, ok, err := validateSession(c)
	if err != nil {
		c.Logger().Errorf("error validateSession: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if !ok || user == nil {
		return errorResponse(c, 401, "login required")
	}
	// ?????????user????????????????????????,???????????????403
	if !isAdminUser(user.Account) {
		return errorResponse(c, 403, "not admin user")
	}

	var adminPlayerBanRequest AdminPlayerBanRequest
	if err := c.Bind(&adminPlayerBanRequest); err != nil {
		c.Logger().Errorf("error Bind request to AdminPlayerBanRequest: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	userAccount := adminPlayerBanRequest.UserAccount
	isBan := adminPlayerBanRequest.IsBan

	ctx := c.Request().Context()
	conn, err := db.Connx(ctx)
	if err != nil {
		c.Logger().Errorf("error db.Conn: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx,
		"UPDATE user SET `is_ban` = ?  WHERE `account` = ?",
		isBan, userAccount,
	); err != nil {
		c.Logger().Errorf("error Update user by is_ban=%t, account=%s: %s", isBan, userAccount, err)
		return errorResponse(c, 500, "internal server error")
	}
	updatedUser, err := getUserByAccount(ctx, conn, userAccount)
	if err != nil {
		c.Logger().Errorf("error getUserByAccount: %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if updatedUser == nil {
		return errorResponse(c, 400, "user not found")
	}

	body := AdminPlayerBanResponse{
		BasicResponse: BasicResponse{
			Result: true,
			Status: 200,
		},
		UserAccount: updatedUser.Account,
		DisplayName: updatedUser.DisplayName,
		IsBan:       updatedUser.IsBan,
		CreatedAt:   updatedUser.CreatedAt,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}

func isAdminUser(account string) bool {
	return account == "adminuser"
}

// ??????????????????API
// DB??????????????????
// auto generated dump data 20220424_0851 size prod
func initializeHandler(c echo.Context) error {
	lastCreatedAt := "2022-05-13 09:00:00.000"
	ctx := c.Request().Context()

	conn, err := db.Connx(ctx)
	if err != nil {
		return errorResponse(c, 500, "internal server error")
	}
	defer conn.Close()

	if _, err := conn.ExecContext(
		ctx,
		"DELETE FROM user WHERE ? < `created_at`",
		lastCreatedAt,
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	if _, err := conn.ExecContext(
		ctx,
		"DELETE FROM playlist WHERE ? < created_at OR user_account NOT IN (SELECT account FROM user)",
		lastCreatedAt,
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	if _, err := conn.ExecContext(
		ctx,
		"DELETE FROM playlist_song WHERE playlist_id NOT IN (SELECT id FROM playlist)",
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	if _, err := conn.ExecContext(
		ctx,
		"DELETE FROM playlist_favorite WHERE playlist_id NOT IN (SELECT id FROM playlist) OR ? < created_at",
		lastCreatedAt,
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	if _, err := conn.ExecContext(
		ctx,
		"DROP TABLE IF EXISTS favorite_count",
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if _, err := conn.ExecContext(
		ctx,
		"CREATE TABLE favorite_count (`playlist_id` bigint NOT NULL, `count` int NOT NULL,"+
			"	PRIMARY KEY (`playlist_id`), KEY `idx_count` (`count` DESC))",
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}
	if _, err := conn.ExecContext(
		ctx,
		"INSERT INTO favorite_count (`playlist_id`, `count`) "+
			"	SELECT playlist_id, count(*) AS count FROM playlist_favorite GROUP BY playlist_id",
	); err != nil {
		c.Logger().Errorf("error: initialize %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	body := BasicResponse{
		Result: true,
		Status: 200,
	}
	if err := c.JSON(http.StatusOK, body); err != nil {
		c.Logger().Errorf("error returns JSON: %s", err)
		return errorResponse(c, 500, "internal server error")
	}

	return nil
}
