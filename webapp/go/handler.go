package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/pkg/errors"
	"golang.org/x/crypto/bcrypt"
)

type Session struct {
	User *User
}

type OKResp struct {
	OK    bool   `jon:"ok"`
	Error string `jon:"error,omitempty"`
}

func NewServer(db *sql.DB, store sessions.Store) http.Handler {
	server := http.NewServeMux()

	h := &Handler{
		db:    db,
		store: store,
	}

	server.HandleFunc("/initialize", h.Initialize)
	server.HandleFunc("/signup", h.Signup)
	server.HandleFunc("/signin", h.Signin)
	server.HandleFunc("/signout", h.Signout)
	server.HandleFunc("/mypage", h.MyPage)
	server.HandleFunc("/sell_orders", h.SellOrders)
	server.HandleFunc("/buy_orders", h.BuyOrders)
	server.HandleFunc("/trades", h.Trades)

	// default 404
	server.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[INFO] request not found %s", r.URL.RawPath)
		http.Error(w, "Not found", 404)
	})

	return h.common(server)
}

type Handler struct {
	db    *sql.DB
	store sessions.Store
}

func (h *Handler) Initialize(w http.ResponseWriter, r *http.Request) {
	err := h.txScorp(func(tx *sql.Tx) error {
		query := `INSERT INTO setting (key, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value = VALUES(value)`
		for _, k := range []string{
			BankEndpoint,
			BankAppid,
			LogEndpoint,
			LogAppid,
		} {
			if _, err := tx.Exec(query, k, r.FormValue(k)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		h.handleError(w, err, http.StatusInternalServerError)
	} else {
		fmt.Fprintln(w, "ok")
	}
}

func (h *Handler) Signup(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		name := r.FormValue("name")
		bankID := r.FormValue("bank_id")
		password := r.FormValue("password")
		if name == "" || bankID == "" || password == "" {
			h.handleError(w, errors.New("all paramaters are required"), http.StatusBadRequest)
			return
		}
		isubank, err := h.newIsubank()
		if err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		logger, err := h.newLogger()
		if err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		// bankIDの検証
		if err = isubank.Check(bankID, 1); err != nil {
			h.handleError(w, err, http.StatusBadRequest)
			return
		}
		pass, err := bcrypt.GenerateFromPassword([]byte(password), 31)
		if err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		if res, err := h.db.Exec(`INSERT INTO user (bank_id, name, password, created_at) VALUES (?, ?, ? NOW())`, bankID, name, pass); err != nil {
			if mysqlError, ok := err.(*mysql.MySQLError); ok {
				if mysqlError.Number == 1062 {
					h.handleError(w, errors.New("bank_id already exists"), http.StatusBadRequest)
					return
				}
			}
			h.handleError(w, err, http.StatusInternalServerError)
			return
		} else {
			userID, _ := res.LastInsertId()
			logger.Send("signup", LogDataSignup{
				BankID: bankID,
				UserID: userID,
				Name:   name,
			})
		}
		http.Redirect(w, r, "/signin", http.StatusFound)
	} else {
		// TODO Signup form or error
	}
}

func (h *Handler) Signin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		bankID := r.FormValue("bank_id")
		password := r.FormValue("password")
		if bankID == "" || password == "" {
			h.handleError(w, errors.New("all paramaters are required"), http.StatusBadRequest)
			return
		}
		logger, err := h.newLogger()
		if err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}

		var userID int64
		pass := []byte{}
		if err := h.db.QueryRow(`SELECT id, password FROM user WHERE bank_id = ?`, bankID).Scan(&userID, &pass); err != nil {
			if err == sql.ErrNoRows {
				h.handleError(w, errors.New("bank_idまたはpasswordが間違っています"), http.StatusNotFound)
				return
			}
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		if err := bcrypt.CompareHashAndPassword(pass, []byte(password)); err != nil {
			if err == bcrypt.ErrMismatchedHashAndPassword {
				h.handleError(w, errors.New("bank_idまたはpasswordが間違っています"), http.StatusNotFound)
				return
			}
			h.handleError(w, err, http.StatusBadRequest)
			return
		}
		session, err := h.store.Get(r, SessionName)
		if err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		session.Values["user_id"] = userID
		if err = session.Save(r, w); err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		logger.Send("signin", LogDataSignin{
			UserID: userID,
		})
		http.Redirect(w, r, "/mypage", http.StatusFound)
	} else {
		// TODO Signin form or error
	}
}

func (h *Handler) Signout(w http.ResponseWriter, r *http.Request) {
	session, err := h.store.Get(r, SessionName)
	if err != nil {
		h.handleError(w, err, http.StatusInternalServerError)
		return
	}
	session.Values["user_id"] = 0
	session.Options = &sessions.Options{MaxAge: -1}
	if err = session.Save(r, w); err != nil {
		h.handleError(w, err, http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/signin", http.StatusFound)
}

func (h *Handler) MyPage(w http.ResponseWriter, r *http.Request) {
	s, err := h.auth(r)
	if err != nil {
		h.handleError(w, err, http.StatusUnauthorized)
		return
	}
	_ = s
}

type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	BankID    string    `json:"-"`
	CreatedAt time.Time `json:"-"`
}

type Trade struct {
	ID       int64      `json:"id"`
	Amount   int64      `json:"amount"`
	Price    int64      `json:"price"`
	ClosedAt *time.Time `json:"closed_at"`
}

type Order struct {
	ID        int64      `json:"id"`
	UserID    int64      `json:"user_id"`
	Amount    int64      `json:"amount"`
	Price     int64      `json:"price"`
	ClosedAt  *time.Time `json:"closed_at"`
	TradeID   int64      `json:"trade_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	User      *User      `json:"user,omitempty"`
	Trade     *Trade     `json:"trade,omitempty"`
}

type SellOrder struct {
	Order
}

type BuyOrder struct {
	Order
}

func (h *Handler) SellOrders(w http.ResponseWriter, r *http.Request) {
	s, err := h.auth(r)
	if err != nil {
		h.handleError(w, err, http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost {
		res := &OKResp{
			OK: true,
		}
		err := h.txScorp(func(tx *sql.Tx) error {
			if _, err := h.lockUser(tx, s.User.ID); err != nil {
				return err
			}
			logger, err := h.newLogger()
			if err != nil {
				return err
			}
			amount, err := formvalInt64(r, "amount")
			if err != nil {
				return err
			}
			price, err := formvalInt64(r, "price")
			if err != nil {
				return err
			}
			res, err := tx.Exec(`INSERT INTO sell_order (user_id, amount, price, created_at) VALUES (?, ?, ?, NOW())`, s.User.ID, amount, price)
			if err != nil {
				return errors.Wrap(err, "insert sell_order failed")
			}
			sellID, err := res.LastInsertId()
			if err != nil {
				return errors.Wrap(err, "get sell_id failed")
			}
			err = logger.Send("sell.order", LogDataSellOrder{
				SellID: sellID,
				UserID: s.User.ID,
				Amount: amount,
				Price:  price,
			})
			if err != nil {
				return errors.Wrap(err, "send log failed")
			}
			return nil
		})
		if err != nil {
			res.OK = false
			res.Error = err.Error() // TODO message
		} else {
			h.runTrade()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(res)
	} else {
		// TODO list API
	}
}

func (h *Handler) BuyOrders(w http.ResponseWriter, r *http.Request) {
	s, err := h.auth(r)
	if err != nil {
		h.handleError(w, err, http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost {
		res := &OKResp{
			OK: true,
		}
		err := h.txScorp(func(tx *sql.Tx) error {
			if _, err := h.lockUser(tx, s.User.ID); err != nil {
				return err
			}
			logger, err := h.newLogger()
			if err != nil {
				return err
			}
			isubank, err := h.newIsubank()
			if err != nil {
				return err
			}
			amount, err := formvalInt64(r, "amount")
			if err != nil {
				return err
			}
			price, err := formvalInt64(r, "price")
			if err != nil {
				return err
			}
			totalPrice := price * amount
			if err = isubank.Check(s.User.BankID, totalPrice); err != nil {
				logger.Send("buy.error", LogDataBuyError{
					Error:  err.Error(),
					UserID: s.User.ID,
					Amount: amount,
					Price:  price,
				})
				if err == ErrCreditInsufficient {
					return errors.New("銀行残高が足りません")
				}
				return errors.Wrap(err, "isubank check failed")
			}
			res, err := tx.Exec(`INSERT INTO buy_order (user_id, amount, price, created_at) VALUES (?, ?, ?, NOW())`, s.User.ID, amount, price)
			if err != nil {
				return errors.Wrap(err, "insert buy_order failed")
			}
			buyID, err := res.LastInsertId()
			if err != nil {
				return errors.Wrap(err, "get buy_id failed")
			}
			err = logger.Send("buy.order", LogDataBuyOrder{
				BuyID:  buyID,
				UserID: s.User.ID,
				Amount: amount,
				Price:  price,
			})
			if err != nil {
				return errors.Wrap(err, "send log failed")
			}
			return nil
		})
		if err != nil {
			res.OK = false
			res.Error = err.Error() // TODO message
		} else {
			h.runTrade()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(res)
	} else {
		// TODO list API
	}
}

func (h *Handler) Trades(w http.ResponseWriter, r *http.Request) {
	s, err := h.auth(r)
	if err != nil {
		h.handleError(w, err, http.StatusUnauthorized)
		return
	}
	_ = s
	// TODO List API
}

func (h *Handler) runTrade() {
	isubank, err := h.newIsubank()
	if err != nil {
		log.Printf("[WARN] isubank init failed. err:%s", err)
		return
	}
	logger, err := h.newLogger()
	if err != nil {
		log.Printf("[WARN] logger init failed. err:%s", err)
		return
	}
	errNoItem := errors.New("no item")
	// TODO Trade
	err = h.txScorp(func(tx *sql.Tx) error {
		// 一番安い売り注文
		sell := SellOrder{}
		q := `SELECT id FROM sell_order WHERE closed_at IS NULL ORDER BY price ASC LIMIT 1`
		err := tx.QueryRow(q).Scan(&sell.ID)
		switch {
		case err == sql.ErrNoRows:
			return errNoItem
		case err != nil:
			return err
		}
		q = `SELECT id, user_id, amount, price FROM sell_order WHERE id = ? FOR UPDATE`
		if err = tx.QueryRow(q, sell.ID).Scan(&sell.ID, &sell.UserID, &sell.Amount, &sell.Price, &sell.ClosedAt); err != nil {
			return err
		}
		if sell.ClosedAt != nil && !sell.ClosedAt.IsZero() {
			// 成約済み
			return nil
		}
		seller, err := h.lockUser(tx, sell.UserID)
		if err != nil {
			return err
		}
		_ = seller
		restAmount := sell.Amount
		// 買い注文
		q = `SELECT id, user_id, amount, price FROM buy_order WHERE closed_at IS NULL AND price >= ? ORDER BY price DESC`
		rows, err := tx.Query(q, sell.Price)
		if err != nil {
			return err
		}
		reserves := []int64{}
		buys := []BuyOrder{}
		defer rows.Close()
		for rows.Next() {
			buy := BuyOrder{}
			if err = rows.Scan(&buy.ID, &buy.UserID, &buy.Amount, &buy.Price); err != nil {
				return err
			}
			q = `SELECT id, closed_at FROM buy_order WHERE id = ? FOR UPDATE`
			if err = tx.QueryRow(q, buy.ID).Scan(&buy.ID, &buy.ClosedAt); err != nil {
				return err
			}
			if buy.ClosedAt != nil && !buy.ClosedAt.IsZero() {
				// 成約済み
				continue
			}
			if buy.Amount > restAmount {
				// TODO 本当はその場合次の売り注文を見たい
				continue
			}
			buyer, err := h.lockUser(tx, buy.UserID)
			if err != nil {
				return err
			}
			resID, err := isubank.Reserve(buyer.BankID, -sell.Price*buy.Amount)
			if err != nil {
				if err == ErrCreditInsufficient {
					// 与信確保失敗
					continue
				}
				return err
			}
			reserves = append(reserves, resID)
			buys = append(buys, buy)
			restAmount -= buy.Amount
			if restAmount == 0 {
				break
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if restAmount > 0 {
			if len(reserves) > 0 {
				if err = isubank.Cancel(reserves); err != nil {
					return err
				}
			}
			return errNoItem
		}
		resID, err := isubank.Reserve(seller.BankID, sell.Price*sell.Amount)
		if err != nil {
			return err
		}
		reserves = append(reserves, resID)
		now := time.Now()
		res, err := tx.Exec(`INSERT INTO trade (amount, price, created_at) VALUES (?, ?, ?)`, sell.Amount, sell.Price, now)
		if err != nil {
			return err
		}
		tradeID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		logger.Send("close", LogDataClose{
			Price:   sell.Price,
			Amount:  sell.Amount,
			TradeID: tradeID,
		})
		for _, buy := range buys {
			if _, err = tx.Exec(`UPDATE buy_order SET trade_id = ? AND closed_at = ? WHERE id = ?`, tradeID, now, buy.ID); err != nil {
				return err
			}
			logger.Send("buy.close", LogDataBuyClose{
				BuyID:   buy.ID,
				Price:   sell.Price,
				Amount:  buy.Amount,
				UserID:  buy.UserID,
				TradeID: tradeID,
			})
		}
		if _, err = tx.Exec(`UPDATE sell_order SET trade_id = ? AND closed_at = ? WHERE id = ?`, tradeID, now, sell.ID); err != nil {
			return err
		}
		logger.Send("sell.close", LogDataSellClose{
			SellID:  sell.ID,
			Price:   sell.Price,
			Amount:  sell.Amount,
			UserID:  sell.UserID,
			TradeID: tradeID,
		})
		if err = isubank.Commit(reserves); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		log.Printf("[WARN] runTrade failed. err: %s", err)
	}
}

func (h *Handler) common(f http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				h.handleError(w, err, http.StatusBadRequest)
				return
			}
		}
		session, err := h.store.Get(r, SessionName)
		if err != nil {
			h.handleError(w, err, http.StatusInternalServerError)
			return
		}
		if _userID, ok := session.Values["user_id"]; ok {
			userID := _userID.(int64)
			user := &User{}
			ctx := r.Context()
			q := `SELECT id, bank_id, name, created_at FROM user WHERE id = ?`
			if err := h.db.QueryRow(q, userID).Scan(&user.ID, &user.BankID, &user.Name, &user.CreatedAt); err != nil {
				h.handleError(w, err, http.StatusInternalServerError)
				return
			}
			ctx = context.WithValue(ctx, "session", &Session{user})
			f.ServeHTTP(w, r.WithContext(ctx))
		} else {
			f.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) auth(r *http.Request) (*Session, error) {
	v := r.Context().Value("session")
	if s, ok := v.(*Session); ok {
		return s, nil
	}
	return nil, errors.New("Not authenticate")
}

func (h *Handler) handleError(w http.ResponseWriter, err error, code int) {
	log.Printf("[WARN] err: %s", err.Error())
	// TODO Error Message
	http.Error(w, err.Error(), code)
}

func (h *Handler) txScorp(f func(*sql.Tx) error) (err error) {
	tx, err := h.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin transaction failed")
	}
	defer func() {
		if e := recover(); e != nil {
			tx.Rollback()
			err = errors.Errorf("panic in transaction: %s", e)
		} else if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	err = f(tx)
	return
}

func (h *Handler) lockUser(tx *sql.Tx, userID int64) (*User, error) {
	user := User{}
	err := tx.QueryRow(`SELECT id, name, bank_id, created_at FROM user WHERE id = ? FOR UPDATE`, userID).Scan(&user.ID, &user.Name, &user.BankID, &user.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (h *Handler) newIsubank() (*Isubank, error) {
	ep, err := h.getSetting(BankEndpoint)
	if err != nil {
		return nil, err
	}
	id, err := h.getSetting(BankAppid)
	if err != nil {
		return nil, err
	}
	return NewIsubank(ep, id)
}

func (h *Handler) newLogger() (*Logger, error) {
	ep, err := h.getSetting(LogEndpoint)
	if err != nil {
		return nil, err
	}
	id, err := h.getSetting(LogAppid)
	if err != nil {
		return nil, err
	}
	return NewLogger(ep, id)
}

func (h *Handler) getSetting(k string) (v string, err error) {
	err = h.db.QueryRow(`SELECT value FROM setting WHERE key = ?`, k).Scan(&v)
	return
}

func formvalInt64(r *http.Request, key string) (int64, error) {
	v := r.FormValue(key)
	if v == "" {
		return 0, errors.Errorf("%s is required", key)
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("[INFO] can't parse to int64 key:%s val:%s err:%s", key, v, err)
		return 0, errors.Errorf("%s can't parse to int64")
	}
	return i, nil
}
