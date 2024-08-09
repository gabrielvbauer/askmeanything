package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gabrielvbauer/askmeanything/server/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

type apiHandlerStruct struct {
	queries     *pgstore.Queries
	router      *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mutex       *sync.Mutex
}

func (http apiHandlerStruct) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	http.router.ServeHTTP(writer, request)
}

func NewHandler(queries *pgstore.Queries) http.Handler {
	apiHandler := apiHandlerStruct{
		queries:     queries,
		upgrader:    websocket.Upgrader{CheckOrigin: func(request *http.Request) bool { return true }},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mutex:       &sync.Mutex{},
	}

	router := chi.NewRouter()
	router.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	router.Get("/subscribe/{room_id}", apiHandler.handleSubscribe)

	router.Route("/api", func(route chi.Router) {
		route.Route("/rooms", func(route chi.Router) {
			route.Post("/", apiHandler.handleCreateRoom)
			route.Get("/", apiHandler.handleGetRooms)

			route.Route("/{room_id}", func(route chi.Router) {
				route.Get("/", apiHandler.handleGetRoom)

				route.Route("/messages", func(route chi.Router) {
					route.Post("/", apiHandler.handleCreateRoomMessage)
					route.Get("/", apiHandler.handleGetRoomMessages)

					route.Route("/{message_id}", func(route chi.Router) {
						route.Get("/", apiHandler.handleGetRoomMessage)
						route.Patch("/react", apiHandler.handleReactToMessage)
						route.Delete("/react", apiHandler.handleRemoveReactFromMessage)
						route.Patch("/answer", apiHandler.handleMarkMessageAsAnswered)
					})
				})
			})
		})
	})

	apiHandler.router = router
	return apiHandler
}

const (
	MessageKindMessageCreated          = "message_created"
	MessageKindMessageRactionIncreased = "message_reaction_increased"
	MessageKindMessageRactionDecreased = "message_reaction_decreased"
	MessageKindMessageAnswered         = "message_answered"
)

type MessageMessageReactionIncreased struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type MessageMessageReactionDecreased struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type MessageMessageAnswered struct {
	ID string `json:"id"`
}

type MessageMessageCreated struct {
	Id      string `json:"id"`
	Message string `json:"message"`
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomId string `json:"-"`
}

func (apiHandler apiHandlerStruct) notifyClient(message Message) {
	apiHandler.mutex.Lock()
	defer apiHandler.mutex.Unlock()

	subscribers, ok := apiHandler.subscribers[message.RoomId]
	if !ok || len(subscribers) == 0 {
		return
	}

	for connection, cancel := range subscribers {
		if err := connection.WriteJSON(message); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
}

func (apiHandler apiHandlerStruct) handleSubscribe(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = apiHandler.queries.GetRoom(request.Context(), roomId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(writer, "room not found", http.StatusBadRequest)
			return
		}

		http.Error(writer, "Something went wrong", http.StatusInternalServerError)
		return
	}

	connection, err := apiHandler.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(writer, "Failed to upgrade to websocket connection", http.StatusBadRequest)
		return
	}

	defer connection.Close()

	ctx, cancel := context.WithCancel(request.Context())

	apiHandler.mutex.Lock()
	if _, ok := apiHandler.subscribers[rawRoomId]; !ok {
		apiHandler.subscribers[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("New client connected", "room_id", rawRoomId, "client_ip", request.RemoteAddr)
	apiHandler.subscribers[rawRoomId][connection] = cancel
	apiHandler.mutex.Unlock()

	<-ctx.Done()

	apiHandler.mutex.Lock()

	delete(apiHandler.subscribers[rawRoomId], connection)

	apiHandler.mutex.Unlock()
}

func (apiHandler apiHandlerStruct) handleCreateRoom(writer http.ResponseWriter, request *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}

	roomId, err := apiHandler.queries.InsertRoom(request.Context(), body.Theme)
	if err != nil {
		slog.Error("failed to inser room", "error", err)
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Id string `json:"id"`
	}

	data, _ := json.Marshal(response{Id: roomId.String()})
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(data)
}

func (apiHandler apiHandlerStruct) handleGetRooms(writer http.ResponseWriter, request *http.Request) {
	rooms, err := apiHandler.queries.GetRooms(request.Context())
	if err != nil {
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
		slog.Error("failed to get rooms", "error", err)
		return
	}

	if rooms == nil {
		rooms = []pgstore.Room{}
	}

	sendJSON(writer, rooms)
}

func (h apiHandlerStruct) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	room, _, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	sendJSON(w, room)
}

func (apiHandler apiHandlerStruct) handleCreateRoomMessage(writer http.ResponseWriter, request *http.Request) {
	rawRoomId := chi.URLParam(request, "room_id")
	roomId, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(writer, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = apiHandler.queries.GetRoom(request.Context(), roomId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(writer, "room not found", http.StatusBadRequest)
			return
		}

		http.Error(writer, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}

	messageId, err := apiHandler.queries.InsertMessage(request.Context(), pgstore.InsertMessageParams{RoomID: roomId, Message: body.Message})
	if err != nil {
		slog.Error("failed to inser message", "error", err)
		http.Error(writer, "something went wrong", http.StatusInternalServerError)
	}

	type response struct {
		Id string `json:"id"`
	}

	data, _ := json.Marshal(response{Id: messageId.String()})
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(data)

	go apiHandler.notifyClient(Message{
		Kind:   MessageKindMessageCreated,
		RoomId: rawRoomId,
		Value: MessageMessageCreated{
			Id:      messageId.String(),
			Message: body.Message,
		},
	})
}

func (h apiHandlerStruct) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	_, _, roomID, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	messages, err := h.queries.GetRoomMessages(r.Context(), roomID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Error("failed to get room messages", "error", err)
		return
	}

	if messages == nil {
		messages = []pgstore.Message{}
	}

	sendJSON(w, messages)
}

func (h apiHandlerStruct) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, _, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawMessageID := chi.URLParam(r, "message_id")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	messages, err := h.queries.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}

		slog.Error("failed to get message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	sendJSON(w, messages)
}

func (h apiHandlerStruct) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawID := chi.URLParam(r, "message_id")
	id, err := uuid.Parse(rawID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	count, err := h.queries.ReactToMessage(r.Context(), id)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Error("failed to react to message", "error", err)
		return
	}

	type response struct {
		Count int64 `json:"count"`
	}

	sendJSON(w, response{Count: count})

	go h.notifyClient(Message{
		Kind:   MessageKindMessageRactionIncreased,
		RoomId: rawRoomID,
		Value: MessageMessageReactionIncreased{
			ID:    rawID,
			Count: count,
		},
	})
}

func (h apiHandlerStruct) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawID := chi.URLParam(r, "message_id")
	id, err := uuid.Parse(rawID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	count, err := h.queries.RemoveReactionFromMessage(r.Context(), id)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Error("failed to react to message", "error", err)
		return
	}

	type response struct {
		Count int64 `json:"count"`
	}

	sendJSON(w, response{Count: count})

	go h.notifyClient(Message{
		Kind:   MessageKindMessageRactionDecreased,
		RoomId: rawRoomID,
		Value: MessageMessageReactionDecreased{
			ID:    rawID,
			Count: count,
		},
	})
}

func (h apiHandlerStruct) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	_, rawRoomID, _, ok := h.readRoom(w, r)
	if !ok {
		return
	}

	rawID := chi.URLParam(r, "message_id")
	id, err := uuid.Parse(rawID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	err = h.queries.MarkMessageAsAnswered(r.Context(), id)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		slog.Error("failed to react to message", "error", err)
		return
	}

	w.WriteHeader(http.StatusOK)

	go h.notifyClient(Message{
		Kind:   MessageKindMessageAnswered,
		RoomId: rawRoomID,
		Value: MessageMessageAnswered{
			ID: rawID,
		},
	})
}
