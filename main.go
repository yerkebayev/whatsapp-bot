package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	client          *whatsmeow.Client
	log             waLog.Logger
	logLevel        = "INFO"
	debugLogs       = flag.Bool("debug", false, "Enable debug logs?")
	dbDialect       = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
	dbAddress       = flag.String("db-address", "./whatsapp.db?_foreign_keys=on", "Database address")
	requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")
	pairRejectChan  = make(chan bool, 1)
	historySyncID   int32
	startupTime     = time.Now()
	mainPhone       = "77009809778"
)

var db *sql.DB

type Message struct {
	ID        int64
	messageID string
	language string
	addressId string
	phone     string
	msgGoodOrBad string
	msgType   string
	text      string
	fileId    string
    answerForMessageId string
	dateTime  string
}

type Address struct {
	addressId string
	title string
	link string
}

type ClientState struct {
	Phone        string
	CurrentStep  string // "choose_language", "choose_address", "choose_type", "completed"
	Language     string
	AddressID    string
	MsgGoodOrBad string
	LastMessage  string
	LastActivity time.Time // track last message time
}

const conversationTimeout = 2 * time.Minute


func main() {
	fmt.Println("Starting WhatsApp bot...")

	waBinary.IndentXML = true

	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
	}
	log = waLog.Stdout("Main", logLevel, true)

	dbLog := waLog.Stdout("Database", logLevel, true)
	ctx := context.Background()
	storeContainer, err := sqlstore.New(ctx, *dbDialect, *dbAddress, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}

	if err := initDB(); err != nil {
		log.Errorf("Failed to initialize database: %v", err)
		return
	}

	device, err := storeContainer.GetFirstDevice(ctx)
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	client = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))
	if client == nil {
		log.Errorf("WhatsApp client is not initialized")
		return
	}

	var isWaitingForPair atomic.Bool
	client.PrePairCallback = func(jid types.JID, platform, businessName string) bool {
		isWaitingForPair.Store(true)
		defer isWaitingForPair.Store(false)
		log.Infof("Pairing %s (platform: %q, business name: %q). Type r within 3 seconds to reject pair", jid, platform, businessName)
		select {
		case reject := <-pairRejectChan:
			if reject {
				log.Infof("Rejecting pair")
				return false
			}
		case <-time.After(3 * time.Second):
		}
		log.Infof("Accepting pair")
		return true
	}

	// Ensure the QR channel is set up before connecting
	if client.Store.ID == nil {
		log.Infof("Attempting to get QR channel")
		ch, err := client.GetQRChannel(context.Background())
		if err != nil {
			log.Errorf("Failed to get QR channel: %v", err)
			return
		}
		go func() {
			for evt := range ch {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)

					//// Save QR code as text in a .txt file
					//txtFile, err := os.Create("qr_code.txt")
					//if err != nil {
					//	log.Errorf("Failed to create text file: %v", err)
					//	return
					//}
					//defer txtFile.Close()
					//_, err = txtFile.WriteString(evt.Code)
					//if err != nil {
					//	log.Errorf("Failed to write to text file: %v", err)
					//	return
					//}
					//
					//// Save QR code as an image in a .png file
					//err = qrcode.WriteFile(evt.Code, qrcode.Medium, 256, "qr_code.png")
					//if err != nil {
					//	log.Errorf("Failed to save QR code image: %v", err)
					//	return
					//}

					log.Infof("QR code saved as text and image")

					log.Infof("QR code sent to /new-qr successfully")
				} else {
					log.Infof("QR channel result: %s", evt.Event)
				}
			}
		}()
	} else {
		log.Infof("Client is already logged in, no need to get QR code")
	}

	log.Infof("Attempting to connect the client")
	err = client.Connect()
	if err != nil {
		log.Errorf("Failed to connect: %v", err)
		return
	}

	client.AddEventHandler(receiveHandler)

	// Start HTTP server
	router := http.NewServeMux()
	router.HandleFunc("/send", sendMessageHandler)
	router.HandleFunc("/get-all-messages", getAllMessagesHandler) // Новый маршрут для получения всех сообщений
	router.HandleFunc("/check-user", checkUserHandler)            // Новый маршрут для проверки номера телефона
	router.HandleFunc("/get-media", mediaHandler)                 // Новый маршрут для получение файлов
	router.HandleFunc("/delete-media", mediaDeleteHandler)        // Удаление файла файлов

	go http.ListenAndServe(":8080", router)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run checker every 1 minute
	go clientStateChecker(ctx, 15*time.Second, client)

	if os.Getenv("DOCKER_MODE") == "true" {
		select {} // Prevent the application from exiting in Docker mode
	} else {
		c := make(chan os.Signal, 1)
		input := make(chan string)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			defer close(input)
			scan := bufio.NewScanner(os.Stdin)
			for scan.Scan() {
				line := strings.TrimSpace(scan.Text())
				if len(line) > 0 {
					input <- line
				}
			}
		}()
		for {
			select {
			case <-c:
				log.Infof("Interrupt received, exiting")
				client.Disconnect()
				return
			case cmd := <-input:
				if len(cmd) == 0 {
					log.Infof("Stdin closed, exiting")
					client.Disconnect()
					return
				}
				if isWaitingForPair.Load() {
					if cmd == "r" {
						pairRejectChan <- true
					} else if cmd == "a" {
						pairRejectChan <- false
					}
					continue
				}
				args := strings.Fields(cmd)
				cmd = args[0]
				args = args[1:]
				go handleCmd(strings.ToLower(cmd), args)
			}
		}
	}
}

func getAddresses() ([]Address, error) {
    // Run query
    rows, err := db.Query("SELECT address_id, title, link FROM addresses")
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var addresses []Address
    for rows.Next() {
		var addr Address
		err := rows.Scan(&addr.addressId, &addr.title, &addr.link)
		if err != nil {
			return nil, err
		}
	
		addr.title = extractStreetAndNumber(addr.title)
		fmt.Println("Street & number:", addr.title)
	
		addresses = append(addresses, addr)
	}
	

    // Check iteration errors
    if err := rows.Err(); err != nil {
        return nil, err
    }

    return addresses, nil
}

func extractStreetAndNumber(full string) string {
    parts := strings.Split(full, " ")
    if len(parts) < 2 {
        return full
    }
    // Return last two tokens, e.g. "Алашахана 34"
    return parts[len(parts)-2] + " " + parts[len(parts)-1]
}


func writeMessageToDB(messageID, jids, msgType, text, dateTime string, state *ClientState) error {
    // Skip if text is empty or whitespace
    if strings.TrimSpace(text) == "" {
        log.Infof("Skipping empty message: %s", messageID)
        return nil
    }

    // Split JIDs string by spaces
    arr := strings.Split(jids, " ")
    if len(arr) < 1 {
        return fmt.Errorf("invalid JIDs string: %s", jids)
    }

    // Parse sender
    sender, ok := parseJID(arr[0])
    if !ok {
        return fmt.Errorf("failed to parse sender JID: %s", arr[0])
    }
    fromPhone := sender.User

    // Parse receiver
    receiverJIDStr := mainPhone
    if len(arr) >= 3 {
        receiverJIDStr = arr[2]
    }

    receiver, ok := parseJID(receiverJIDStr)
    if !ok {
        return fmt.Errorf("failed to parse receiver JID: %s", receiverJIDStr)
    }
    toPhone := receiver.User

    fmt.Printf("Sender: %s, Receiver: %s\n", fromPhone, toPhone)

    // Only save if either sender or receiver is mainPhone
    if fromPhone != mainPhone && toPhone != mainPhone {
        log.Infof("Skipping message not involving main phone: %s", messageID)
        return nil
    }

    addressID := "0" // default
    if state != nil && state.AddressID != "" {
        addressID = state.AddressID
    }

    language := ""
    if state != nil && state.Language != "" {
        language = state.Language
    }

	msgGoodOrBad := ""
	if state != nil && state.MsgGoodOrBad != "" {
        msgGoodOrBad = state.MsgGoodOrBad
    }


    query := `INSERT INTO messages (
        message_id, language, address_id, from_phone, to_phone, msgGoodOrBad, message_type, text, date_time
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

    _, err := db.Exec(query,
        messageID,
        language,
        addressID,
        fromPhone,
        toPhone,
        msgGoodOrBad,
        msgType,
        text,
        dateTime,
    )
    if err != nil {
        log.Errorf("Failed to insert message into database: %v", err)
        return fmt.Errorf("failed to insert message into database: %v", err)
    }

    log.Infof("Message inserted into database: %s", messageID)
    return nil
}



func GetClientState(phone string) (*ClientState, error) {
	row := db.QueryRow(`SELECT phone, current_step, language, address_id, last_message, last_activity FROM client_states WHERE phone = ?`, phone)
	var state ClientState
	var lastActivityStr string
	err := row.Scan(&state.Phone, &state.CurrentStep, &state.Language, &state.AddressID, &state.LastMessage, &lastActivityStr)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	state.LastActivity, _ = time.Parse(time.RFC3339, lastActivityStr)
	return &state, nil
}

func SaveClientState(state *ClientState) error {
	state.LastActivity = time.Now()
	_, err := db.Exec(`
	INSERT INTO client_states (phone, current_step, language, address_id, last_message, last_activity)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(phone) DO UPDATE SET
	current_step=excluded.current_step,
	language=excluded.language,
	address_id=excluded.address_id,
	last_message=excluded.last_message,
	last_activity=excluded.last_activity
	`,
		state.Phone, state.CurrentStep, state.Language, state.AddressID, state.LastMessage, state.LastActivity.Format(time.RFC3339))
	return err
}


func deleteMessageFromDB(messageId string) (bool, error) {
	query := "DELETE FROM messages WHERE message_id=$1"
	_, err := db.Exec(query, messageId)
	if err != nil {
		log.Errorf("Failed to delete message from database: %v", err)
		return false, fmt.Errorf("failed to delete message from database: %v", err)
	}
	return true, nil
}

func sendRecordToCentralDepository(recordCount int) error {
	if mainPhone == "" {
		row, err := db.Query("SELECT jid FROM whatsmeow_device limit 1")
		if err != nil {
			mainPhone = ""
		}
		defer row.Close()
		for row.Next() {
			var jid = ""
			if err := row.Scan(&jid); err != nil {
				mainPhone = ""
			}
			mainPhone = SubstringBefore(jid, ":")
		}
	}
	log.Infof("Jid = %s", mainPhone)
	rows, err := db.Query("SELECT message_id, phone, message_type, text, date_time FROM messages limit ?", recordCount)
	if err != nil {
		return fmt.Errorf("Failed to query messages: %v", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var messageID, phone, msgType, text, dateTime string
		if err := rows.Scan(&messageID, &phone, &msgType, &text, &dateTime); err != nil {
			return fmt.Errorf("Failed to scan message: %v", err)
		}
		messages = append(messages, Message{
			messageID: messageID,
			phone:     phone,
			msgType:   msgType,
			text:      text,
			dateTime:  dateTime,
		})
		log.Infof("Message ID: %s", messageID)
	}
	for _, message := range messages {
		if message.text == "" {
			_, err := deleteMessageFromDB(message.messageID)
			if err != nil {
				log.Errorf("Failed to delete message from database: %v", err)
			}
		} else {
			var clientId = SubstringBefore(message.phone, "@")
			var fromMe = clientId == mainPhone

			if fromMe {
				clientId = SubstringBefore(SubstringAfter(message.phone, " in "), "@")
			}
			if strings.Contains(clientId, ":") {
				clientId = SubstringBefore(message.phone, ":")
			}
			// data := &CDMessage{
			// 	ClientId:    clientId,
			// 	Type:        message.msgType,
			// 	Message:     message.text,
			// 	FromClient:  !fromMe,
			// 	MessageType: "test",
			// }
			// jsonData, err := json.Marshal(data)
			// if err != nil {
			// 	log.Errorf("Failed to generate JSON: %v", err)
			// } else {
			// 	log.Infof("JSON: %s", string(jsonData))

				// r, err := http.NewRequest("POST", *depositoryUrl, bytes.NewBuffer(jsonData))
				// if err != nil {
					// log.Errorf("Failed to create request: %v", err)
				// }

				// r.Header.Add("Content-Type", "application/json")

				// client := &http.Client{}
				// res, err := client.Do(r)
				// if err != nil {
				// 	log.Errorf("Failed to create request: %v", err)
				// }

				// defer res.Body.Close()

				// post := res.Body

				// if res.StatusCode != http.StatusCreated {
				// 	log.Errorf("Failed to create request: %v", err)
				// } else {
				// 	log.Infof("Create record with ID: %s", post)
				// 	_, err := deleteMessageFromDB(message.messageID)
				// 	if err != nil {
				// 		log.Errorf("Failed to delete message from database: %v", err)
				// 	}
				// }

			// }
		}
	}
	messages = nil
	return nil

}

func SubstringBefore(str string, sep string) string {
	idx := strings.Index(str, sep)
	if idx == -1 {
		return str
	}
	return str[:idx]
}

func SubstringAfter(str string, sep string) string {
	idx := strings.Index(str, sep)
	if idx == -1 {
		return str
	}
	return str[idx+len(sep):]
}

func parseJID(arg string) (types.JID, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		log.Errorf("Empty JID string")
		return types.JID{}, false
	}

	// Remove leading '+'
	if strings.HasPrefix(arg, "+") {
		arg = arg[1:]
	}

	// No @ means it's just a user — attach default server
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	}

	// Parse full JID
	recipient, err := types.ParseJID(arg)
	if err != nil {
		log.Errorf("Invalid JID %s: %v", arg, err)
		return types.JID{}, false
	}

	if recipient.User == "" {
		log.Errorf("Invalid JID %s: no user part", arg)
		return types.JID{}, false
	}

	return recipient, true
}

func handleCmd(cmd string, args []string) {
	ctx := context.Background()
	switch cmd {
	case "pair-phone":
		if len(args) < 1 {
			log.Errorf("Usage: pair-phone <number>")
			return
		}
		linkingCode, err := client.PairPhone(ctx, args[0], true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			panic(err)
		}
		fmt.Println("Linking code:", linkingCode)
	case "reconnect":
		client.Disconnect()
		err := client.Connect()
		if err != nil {
			log.Errorf("Failed to connect: %v", err)
		}
	case "logout":
		err := client.Logout(ctx)
		if err != nil {
			log.Errorf("Error logging out: %v", err)
		} else {
			log.Infof("Successfully logged out")
		}
	case "appstate":
		if len(args) < 1 {
			log.Errorf("Usage: appstate <types...>")
			return
		}
		names := []appstate.WAPatchName{appstate.WAPatchName(args[0])}
		if args[0] == "all" {
			names = []appstate.WAPatchName{appstate.WAPatchRegular, appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow, appstate.WAPatchCriticalUnblockLow, appstate.WAPatchCriticalBlock}
		}
		resync := len(args) > 1 && args[1] == "resync"
		for _, name := range names {
			err := client.FetchAppState(ctx, name, resync, false)
			if err != nil {
				log.Errorf("Failed to sync app state: %v", err)
			}
		}
	case "request-appstate-key":
		if len(args) < 1 {
			log.Errorf("Usage: request-appstate-key <ids...>")
			return
		}
		var keyIDs = make([][]byte, len(args))
		for i, id := range args {
			decoded, err := hex.DecodeString(id)
			if err != nil {
				log.Errorf("Failed to decode %s as hex: %v", id, err)
				return
			}
			keyIDs[i] = decoded
		}
		client.DangerousInternals().RequestAppStateKeys(context.Background(), keyIDs)
	case "unavailable-request":
		if len(args) < 3 {
			log.Errorf("Usage: unavailable-request <chat JID> <sender JID> <message ID>")
			return
		}
		chat, ok := parseJID(args[0])
		if !ok {
			return
		}
		sender, ok := parseJID(args[1])
		if !ok {
			return
		}
		resp, err := client.SendMessage(
			context.Background(),
			client.Store.ID.ToNonAD(),
			client.BuildUnavailableMessageRequest(chat, sender, args[2]),
			whatsmeow.SendRequestExtra{Peer: true},
		)
		fmt.Println(resp)
		fmt.Println(err)
	case "checkuser":
		if len(args) < 1 {
			log.Errorf("Usage: checkuser <phone numbers...>")
			return
		}
		resp, err := client.IsOnWhatsApp(args)
		if err != nil {
			log.Errorf("Failed to check if users are on WhatsApp:", err)
		} else {
			for _, item := range resp {
				if item.VerifiedName != nil {
					log.Infof("%s: on whatsapp: %t, JID: %s, business name: %s", item.Query, item.IsIn, item.JID, item.VerifiedName.Details.GetVerifiedName())
				} else {
					log.Infof("%s: on whatsapp: %t, JID: %s", item.Query, item.IsIn, item.JID)
				}
			}
		}

	}
}


func receiveHandler(rawEvt interface{}) {
	ctx := context.Background()
	switch event := rawEvt.(type) {
	case *events.Message:

		if event.Info.Timestamp.Before(startupTime) {
			fmt.Println("skip", event.Message)
			return
		}

		metaParts := []string{
			fmt.Sprintf("pushname: %s", event.Info.PushName),
			fmt.Sprintf("timestamp: %s", event.Info.Timestamp.String()),
		}
		if event.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", event.Info.Type))
		}
		if event.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", event.Info.Category))
		}
		if event.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if event.IsEphemeral {
			metaParts = append(metaParts, "ephemeral")
		}
		if event.IsViewOnceV2 {
			metaParts = append(metaParts, "ephemeral (v2)")
		}
		if event.IsDocumentWithCaption {
			metaParts = append(metaParts, "document with caption")
		}
		if event.IsEdit {
			metaParts = append(metaParts, "edit")
		}

		log.Infof("Received message %s from %s (%s): %+v", event.Info.ID, event.Info.SourceString(), strings.Join(metaParts, ", "), event.Message)

		var filePath, msgType, text string

		// Handle different message types
		if img := event.Message.GetImageMessage(); img != nil {
			data, err := client.Download(ctx, img)
			if err != nil {
				log.Errorf("Failed to download image: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(img.GetMimetype())
			if len(exts) == 0 {
				log.Warnf("Failed to determine file extension for mimetype: %s, using fallback .jpg", img.GetMimetype())
				exts = append(exts, ".jpg")
			}
			filePath = fmt.Sprintf("media/image/%s%s", event.Info.ID, exts[0])
			err = saveMediaFile(filePath, data)
			if err != nil {
				log.Errorf("Failed to save image: %v", err)
				return
			}
			log.Infof("Saved image in message to %s", filePath)
			msgType = "image"
			text = filePath
		} else if vid := event.Message.GetVideoMessage(); vid != nil {
			data, err := client.Download(ctx, vid)
			if err != nil {
				log.Errorf("Failed to download video: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(vid.GetMimetype())
			if len(exts) == 0 {
				log.Warnf("Failed to determine file extension for mimetype: %s, using fallback .mp4", vid.GetMimetype())
				exts = append(exts, ".mp4")
			}
			filePath = fmt.Sprintf("media/video/%s%s", event.Info.ID, exts[0])
			err = saveMediaFile(filePath, data)
			if err != nil {
				log.Errorf("Failed to save video: %v", err)
				return
			}
			log.Infof("Saved video in message to %s", filePath)
			msgType = "video"
			text = filePath
		} else if aud := event.Message.GetAudioMessage(); aud != nil {
			data, err := client.Download(ctx, aud)
			if err != nil {
				log.Errorf("Failed to download audio: %v", err)
				return
			}
			filePath = fmt.Sprintf("media/audio/%s.ogg", event.Info.ID)
			err = saveMediaFile(filePath, data)
			if err != nil {
				log.Errorf("Failed to save audio: %v", err)
				return
			}
			log.Infof("Saved audio in message to %s", filePath)
			msgType = "audio"
			text = filePath
		} else if doc := event.Message.GetDocumentMessage(); doc != nil {
			data, err := client.Download(ctx, doc)
			if err != nil {
				log.Errorf("Failed to download document: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(doc.GetMimetype())
			if len(exts) == 0 {
				log.Warnf("Failed to determine file extension for mimetype: %s, using fallback .pdf", doc.GetMimetype())
				exts = append(exts, ".pdf")
			}
			filePath = fmt.Sprintf("media/document/%s%s", event.Info.ID, exts[0])
			err = saveMediaFile(filePath, data)
			if err != nil {
				log.Errorf("Failed to save document: %v", err)
				return
			}
			log.Infof("Saved document in message to %s", filePath)
			msgType = "document"
			text = filePath
		} else {
			if extMsg := event.Message.GetExtendedTextMessage(); extMsg != nil {
				text = extMsg.GetText()
				// Check for quoted/replied messages
				if quotedMsg := extMsg.GetContextInfo().GetQuotedMessage(); quotedMsg != nil {
					quotedText := quotedMsg.GetConversation()
					text = fmt.Sprintf("%s (replied to: %s)", text, quotedText)
				}
			} else {
				text = event.Message.GetConversation()
			}
			msgType = event.Info.Type
		}
		sender := event.Info.Sender
		if sender.User != mainPhone {
			state, err := GetClientState(sender.User)
			if err != nil {
				log.Errorf("Failed to get client state: %v", err)
				return
	     	}

			startBoolean := false

			 if state == nil {
				state = &ClientState{
					Phone:       sender.User,
					CurrentStep: "choose_language",
				}
				SendMessageTo(client, sender, getMessage("ru", "choose_language"))
				startBoolean = true
			} else {
				// Check timeout
				if time.Since(state.LastActivity) > conversationTimeout {
					state.CurrentStep = "choose_language"
					state.Language = ""
					state.AddressID = ""
					state.MsgGoodOrBad = ""
				    SendMessageTo(client, sender, getMessage("ru", "choose_language"))
					startBoolean = true
				}
			}
			state.LastMessage = text

			if startBoolean == false {
				switch state.CurrentStep {
				case "choose_language":
					lang := parseLanguageChoice(text)
					if lang == "" {
						SendMessageTo(client, sender, getMessage("ru", "invalid_language"))
						return
					}
					state.Language = lang
					state.CurrentStep = "choose_address"

					addresses, err1 := getAddresses()
					if err1 != nil {
						log.Errorf("Failed to get addresses: %v", err)
						return
					 }
					
					// 🔹 Build numbered list
					var msgBuilder strings.Builder
					msgBuilder.WriteString(getMessage(state.Language, "choose_address") + "\n")
					for i, addr := range addresses {
						msgBuilder.WriteString(fmt.Sprintf("%d. %s\n", i+1, addr))
					}
					SendMessageTo(client, sender, msgBuilder.String())
				case "choose_address":
					addrID := text
					if addrID == "" {
						addresses, err1 := getAddresses()
						if err1 != nil {
							log.Errorf("Failed to get addresses: %v", err)
							return
						 }
						
						// 🔹 Build numbered list
						var msgBuilder strings.Builder
						msgBuilder.WriteString(getMessage(state.Language, "invalid_address") + "\n")
						for i, addr := range addresses {
							msgBuilder.WriteString(fmt.Sprintf("%d. %s\n", i+1, addr))
						}
						SendMessageTo(client, sender, msgBuilder.String())
						return
					}
					state.AddressID = addrID
					state.CurrentStep = "choose_type"
					SendMessageTo(client, sender, "Хабарлама түрін таңдаңы:\n1. Good\n2. Hate")
				case "choose_type":
					msgGoodOrBad := text
					if msgGoodOrBad == "" {
						SendMessageTo(client, sender, "Please select a valid message number:\n1. Shop A\n2. Shop B")
						return
					}
					state.MsgGoodOrBad = msgGoodOrBad
					state.CurrentStep = "process"
					SendMessageTo(client, sender, "Write your feedback down! 😊")

					if err := SaveClientState(state); err != nil {
						log.Errorf("Failed to save client state: %v", err)
					}
				case "process":
					println("in process")
					// let's start storing while timeout
				}
			}

			fmt.Printf(
				"Curr state: Phone=%s, CurrentStep=%s, Language=%s, AddressID=%s, MsgGoodOrBad=%s, LastMessage=%s, LastActivity=%s\n",
				state.Phone,
				state.CurrentStep,
				state.Language,
				state.AddressID,
				state.MsgGoodOrBad,
				state.LastMessage,
				state.LastActivity.Format(time.RFC3339),
			)
			

			if state.CurrentStep != "process" {
				if err := SaveClientState(state); err != nil {
					log.Errorf("Failed to save client state: %v", err)
				}
			}

			// Save message to Database
			err = writeMessageToDB(event.Info.ID, event.Info.SourceString(), msgType, text, event.Info.Timestamp.String(), state)

			if err != nil {
				log.Errorf("Failed to write message to database: %v", err)
			}
		}
	}
}

func SendMessageTo(client *whatsmeow.Client, receiver types.JID, text string) {
	msg := &waProto.Message{
		Conversation: proto.String(text),
	}
	_, err := client.SendMessage(context.Background(), receiver, msg)
	if err != nil {
		log.Errorf("Failed to send message to %s: %v", receiver, err)
	}
}

func containsAddress(text string, addresses []Address) *Address {
	for _, addr := range addresses {
		if strings.Contains(strings.ToLower(text), strings.ToLower(addr.title)) || strings.Contains(strings.ToLower(addr.title), strings.ToLower(text)) {
			return &addr
		}
	}
	return nil
}

func mediaDeleteHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
		Path string `json:"path"`
	}

	e := os.Remove("/" + req.Path)
	if e != nil {
		log.Errorf("Failed to delete file: %v", e)
	}

	w.Write([]byte("Ok"))

}

func mediaHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
		Path string `json:"path"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", getExtensionType(req.Type, req.Path))

	http.ServeFile(w, r, "/"+req.Path)
}

func getExtensionType(fileType, filePath string) string {
	parts := strings.Split(filePath, ".")
	ext := strings.ToLower(parts[len(parts)-1])
	switch fileType {
	case "image":
		{
			switch ext {
			case "apng":
				return "image/apng"

			case "avif":
				return "image/avif"

			case "gif", "giff":
				return "image/gif"

			case "jpeg", "jpg":
				return "image/jpeg"

			case "png":
				return "image/png"

			case "svg":
				return "image/svg+xml"

			case "webp":
				return "image/webp"

			case "bmp":
				return "image/bmp"

			case "ico":
				return "image/x-icon"

			case "tiff":
				return "image/tiff"
			default:
				return "image/png"
			}
		}
	case "video":
		{

			switch ext {
			case "mpeg":
				return "video/mpeg"

			case "mp4":
				return "video/mp4"

			case "ogg":
				return "video/ogg"

			case "qt":
				return "video/quicktime"

			case "webm":
				return "video/webm"

			case "wmv":
				return "video/x-ms-wmv"

			case "flv":
				return "video/x-flv"

			case "avi":
				return "video/x-msvideo"

			case "3gpp":
				return "video/3gpp"

			case "3gpp2":
				return "video/3gpp2"
			default:
				return "video/mp4"
			}
		}
	case "audio":
		{
			switch ext {
			case "mulaw":
				return "audio/basic"

			case "l24":
				return "audio/L24"

			case "mp4":
				return "audio/mp4"

			case "aac":
				return "audio/aac"

			case "mpeg", "mp3":
				return "audio/mpeg"

			case "ogg":
				return "audio/ogg"

			case "vorbis":
				return "audio/vorbis"

			case "wma":
				return "audio/x-ms-wma"

			case "wax":
				return "audio/x-ms-wax"

			case "ra":
				return "audio/vnd.rn-realaudio"

			case "wav":
				return "audio/vnd.wave"

			case "webm":
				return "audio/webm"
			default:
				return "audio/webm"
			}
		}
	case "document":
		{
			switch ext {
			case "pdf":
				return "application/pdf"

			case "zip":
				return "application/zip"

			case "csv":
				return "text/csv"

			case "xls", "xlsx":
				return "application/vnd.ms-excel"

			case "ppt", "pptx":
				return "application/vnd.ms-powerpoint"

			case "doc", "docx":
				return "application/msword"

			case "rar":
				return "application/vnd.rar"
			default:
				return "text/html"
			}
		}
	default:
		return "text/html"
	}
}

func sendMessageHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JID       string `json:"jid"`
		Text      string `json:"text,omitempty"`
		ImagePath string `json:"image_path,omitempty"`
		VideoPath string `json:"video_path,omitempty"`
		AudioPath string `json:"audio_path,omitempty"`
		Buttons   []struct {
			DisplayText string `json:"display_text"`
			ButtonID    string `json:"button_id"`
		} `json:"buttons,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	jid, ok := parseJID(req.JID)
	if !ok {
		http.Error(w, "Invalid JID", http.StatusBadRequest)
		return
	}

	var message *waProto.Message
	switch {
	case req.ImagePath != "":
		data, err := os.ReadFile(req.ImagePath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read image file: %v", err), http.StatusInternalServerError)
			return
		}
		uploaded, err := client.Upload(context.Background(), data, whatsmeow.MediaImage)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to upload image: %v", err), http.StatusInternalServerError)
			return
		}
		message = &waProto.Message{
			ImageMessage: &waProto.ImageMessage{
				Caption:       proto.String(req.Text),
				URL:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				Mimetype:      proto.String(http.DetectContentType(data)),
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}
	case req.VideoPath != "":
		data, err := os.ReadFile(req.VideoPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read video file: %v", err), http.StatusInternalServerError)
			return
		}
		uploaded, err := client.Upload(context.Background(), data, whatsmeow.MediaVideo)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to upload video: %v", err), http.StatusInternalServerError)
			return
		}
		message = &waProto.Message{
			VideoMessage: &waProto.VideoMessage{
				Caption:       proto.String(req.Text),
				URL:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				Mimetype:      proto.String(http.DetectContentType(data)),
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}
	case req.AudioPath != "":
		data, err := os.ReadFile(req.AudioPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read audio file: %v", err), http.StatusInternalServerError)
			log.Errorf("Failed to read audio file: %v", err)
			return
		}
		// mp3 => ogg

		log.Infof("Uploading audio file: %s", req.AudioPath)
		uploaded, err := client.Upload(context.Background(), data, whatsmeow.MediaAudio)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to upload audio: %v", err), http.StatusInternalServerError)
			log.Errorf("Failed to upload audio: %v", err)
			return
		}
		log.Infof("Audio uploaded successfully: URL=%s", uploaded.URL)
		message = &waProto.Message{
			AudioMessage: &waProto.AudioMessage{
				URL:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				Mimetype:      proto.String("audio/ogg; codecs=opus"),
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}
	case len(req.Buttons) > 0:
		buttons := make([]*waProto.ButtonsMessage_Button, len(req.Buttons))
		for i, btn := range req.Buttons {
			buttons[i] = &waProto.ButtonsMessage_Button{
				ButtonID: proto.String(btn.ButtonID),
				ButtonText: &waProto.ButtonsMessage_Button_ButtonText{
					DisplayText: proto.String(btn.DisplayText),
				},
				Type: waProto.ButtonsMessage_Button_RESPONSE.Enum(),
			}
		}
		message = &waProto.Message{
			ButtonsMessage: &waProto.ButtonsMessage{
				ContentText: proto.String(req.Text),
				Buttons:     buttons,
				FooterText:  proto.String(""),
				HeaderType:  waProto.ButtonsMessage_TEXT.Enum(),
			},
		}
	default:
		message = &waProto.Message{
			Conversation: proto.String(req.Text),
		}
	}

	resp, err := client.SendMessage(context.Background(), jid, message)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to send message: %v", err), http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}

func saveMediaFile(filePath string, data []byte) error {
	return os.WriteFile(filePath, data, 0600)
}
func getAllMessagesHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT message_id, phone, message_type, text, date_time FROM messages")
	if err != nil {
		http.Error(w, "Failed to query messages", http.StatusInternalServerError)
		log.Errorf("Failed to query messages: %v", err)
		return
	}
	defer rows.Close()

	var messages []map[string]string
	for rows.Next() {
		var messageID, phone, msgType, text, dateTime string
		if err := rows.Scan(&messageID, &phone, &msgType, &text, &dateTime); err != nil {
			http.Error(w, "Failed to scan message", http.StatusInternalServerError)
			log.Errorf("Failed to scan message: %v", err)
			return
		}
		messages = append(messages, map[string]string{
			"message_id":   messageID,
			"phone":        phone,
			"message_type": msgType,
			"text":         text,
			"date_time":    dateTime,
		})
	}

	if err = rows.Err(); err != nil {
		http.Error(w, "Error iterating over rows", http.StatusInternalServerError)
		log.Errorf("Error iterating over rows: %v", err)
		return
	}

	jsonData, err := json.Marshal(messages)
	if err != nil {
		http.Error(w, "Failed to generate JSON", http.StatusInternalServerError)
		log.Errorf("Failed to generate JSON: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonData)
}
func initDB() error {
	var err error
	db, err = sql.Open(*dbDialect, *dbAddress)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %v", err)
	}

	db.Exec("PRAGMA journal_mode=WAL;")
	db.Exec("PRAGMA busy_timeout = 5000;")


	// Create Messages table
	messageTable := `
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT,
		language TEXT,
		address_id TEXT,
		from_phone TEXT,
		to_phone TEXT,
		msgGoodOrBad TEXT,
		message_type TEXT,
		text TEXT,
		file_id TEXT,
		answer_for_message_id TEXT,
		date_time TEXT
	);`

	// Create Addresses table
	addressTable := `
	CREATE TABLE IF NOT EXISTS addresses (
		address_id TEXT PRIMARY KEY,
		title TEXT,
		link TEXT
	);`

	// Create ClientStates table
	stateTable := `
	CREATE TABLE IF NOT EXISTS client_states (
		phone TEXT PRIMARY KEY,
		current_step TEXT,
		language TEXT,
		address_id TEXT,
		msgGoodOrBad TEXT,
		last_message TEXT,
		last_activity DATETIME
	);`

	// Execute table creation
	if _, _ = db.Exec(messageTable); err != nil {
		return fmt.Errorf("failed to create messages table: %v", err)
	}
	if _, _ = db.Exec(addressTable); err != nil {
		return fmt.Errorf("failed to create addresses table: %v", err)
	}
	if _, _ = db.Exec(stateTable); err != nil {
		return fmt.Errorf("failed to create client_states table: %v", err)
	}

	fmt.Println("Database initialized successfully.")
	return nil
}

func checkUserHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PhoneNumber string `json:"phone_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if req.PhoneNumber == "" {
		http.Error(w, "Phone number is required", http.StatusBadRequest)
		return
	}

	resp, err := client.IsOnWhatsApp([]string{req.PhoneNumber})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to check phone number: %v", err), http.StatusInternalServerError)
		return
	}
	println(resp)

	if len(resp) > 0 && resp[0].IsIn {
		jid := resp[0].JID.String()
		phone := strings.SplitN(jid, "@", 2)[0]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"exists":       true,
			"phone_number": phone,
		})
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"exists": false})
	}
}

func parseLanguageChoice(msg string) string {
	switch strings.TrimSpace(msg) {
	case "1":
		return "kk"
	case "2":
		return "ru"
	default:
		return ""
	}
}


func clientStateChecker(ctx context.Context, interval time.Duration, client *whatsmeow.Client) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
			fmt.Println("Start to check")

            rows, err := db.Query(`SELECT phone, current_step, language, address_id, msgGoodOrBad, last_message, last_activity FROM client_states`)
            if err != nil {
                log.Errorf("Failed to query client states: %v", err)
                continue
            }

			defer rows.Close()
            for rows.Next() {
				var state ClientState
				var lastActivityStr string
				var msgGoodOrBad sql.NullString // nullable column
				if err := rows.Scan(
					&state.Phone,
					&state.CurrentStep,
					&state.Language,
					&state.AddressID,
					&msgGoodOrBad,
					&state.LastMessage,
					&lastActivityStr,
				); err != nil {
					log.Errorf("Failed to scan row: %v", err)
					continue
				}
			
				// Convert nullable string to normal string
				if msgGoodOrBad.Valid {
					state.MsgGoodOrBad = msgGoodOrBad.String
				} else {
					state.MsgGoodOrBad = ""
				}
			
				state.LastActivity, _ = time.Parse(time.RFC3339, lastActivityStr)
			
				// Check if client is in process step and inactive for timeout
				if state.CurrentStep == "process" && time.Since(state.LastActivity) > conversationTimeout {
					receiver, ok := parseJID(state.Phone)
					if !ok {
						log.Errorf("Invalid phone/JID: %s", state.Phone)
						continue
					}
					SendMessageTo(client, receiver, getMessage(state.Language, "end_feedback"))
			
					// Reset conversation
					state.CurrentStep = "choose_language"
					state.Language = ""
					state.AddressID = ""
					state.MsgGoodOrBad = ""
					state.LastMessage = ""
					state.LastActivity = time.Now()
			
					if err := SaveClientState(&state); err != nil {
						log.Errorf("Failed to save client state: %v", err)
					}
				}
			}
        }
    }
}


// 1. Define translations
var messages = map[string]map[string]string{
	"kz": {
		"choose_language": "Сәлем! 🌐Қызмет көрсету тілін таңдаңыз\n🌐Выберите язык обслуживания\n\n1. Қазақ тілі\n2. Русский язык",
		"invalid_language": "🌐Ұсынылған сандардың бірін таңдаңыз\n🌐Выберите одну из предложенных цифр\n\n1. Қазақ тілі\n2. Русский язык",
		"choose_address": "Мекенжайды таңдаңыз:",
		"invalid_address": "Өтінемін дұрыс мекенжай нөмірін таңдаңыз:",
		"choose_type": "Хабарлама түрін таңдаңыз:\n1. Шағым\n2. Ұсыныс",
		"invalid_type": "Өтінемін дұрыс нұсқаны таңдаңыз:\n1. Шағым\n2. Ұсыныс",
		"feedback": "Өз пікіріңізді жазыңыз! 😊",
		"end_feedback": "Пікіріңізге рахмет! 😊",
	},
	"ru": {
		"choose_language": "Сәлем! 🌐Қызмет көрсету тілін таңдаңыз\n🌐Выберите язык обслуживания\n\n1. Қазақ тілі\n2. Русский язык",
		"invalid_language": "🌐Ұсынылған сандардың бірін таңдаңыз\n🌐Выберите одну из предложенных цифр\n\n1. Қазақ тілі\n2. Русский язык",
		"choose_address": "Пожалуйста, выберите адрес:",
		"invalid_address": "Пожалуйста, выберите правильный номер адреса:",
		"choose_type": "Выберите тип сообщения:\n1. Жалоба\n2. Предложение",
		"invalid_type": "Пожалуйста, выберите правильный вариант:\n1. Жалоба\n2. Предложение",
		"feedback": "Напишите ваш отзыв! 😊",
		"end_feedback": "Спасибо за ваш отзыв! 😊",
	},
}

func getMessage(lang, key string) string {
	if lang == "" {
		lang = "ru" // default to English
	}
	if val, ok := messages[lang][key]; ok {
		return val
	}
	return messages["ru"][key] // fallback to English
}