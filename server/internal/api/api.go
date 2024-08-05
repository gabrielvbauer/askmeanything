package api

import (
	"net/http"

	"github.com/gabrielvbauer/askmeanything/server/internal/store/pgstore"
	"github.com/go-chi/chi/v5"
)

type apiHandlerStruct struct {
	queries *pgstore.Queries
	router  *chi.Mux
}

func (http apiHandlerStruct) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	http.router.ServeHTTP(writer, request)
}

func NewHandler(queries *pgstore.Queries) http.Handler {
	apiHandler := apiHandlerStruct{
		queries: queries,
	}

	router := chi.NewRouter()

	apiHandler.router = router
	return apiHandler
}
