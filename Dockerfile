# Сборка обоих бинарей: торгующего процесса и сторожа.
#
# Сторож обязан быть отдельным процессом и жить в отдельном контейнере: он
# существует ровно для того случая, когда торгующий процесс умер. Собираются
# они из одного дерева, поэтому образ общий, а роль выбирается командой.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: они меняются заметно реже исходников,
# и пересобирать их на каждую правку кода незачем.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO выключен: бинарь получается статическим и не зависит от библиотек
# базового образа. -trimpath убирает пути сборочной машины из бинаря.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mmbot    ./cmd/mmbot && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/guardian ./cmd/guardian && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/standxctl ./cmd/standxctl

# --- Рабочий образ ---
#
# Alpine, а не distroless, сознательно: в контейнере с торгующим ботом рано
# или поздно придётся разбираться на месте, и наличие оболочки экономит часы.
# Плата за это — доступная оболочка в образе, который держит доступ к счёту,
# поэтому процесс не root и файловая система пишется только в /app/data.
FROM alpine:3.21

# Сертификаты нужны для TLS до биржи, tzdata — чтобы время в логах было UTC,
# а не «локальным временем контейнера», которого не существует.
RUN apk add --no-cache ca-certificates tzdata curl && \
    adduser -D -u 10001 -h /app mmbot

WORKDIR /app
COPY --from=build /out/mmbot /out/guardian /out/standxctl /usr/local/bin/

# Каталоги состояния создаются заранее и принадлежат непривилегированному
# пользователю: том, смонтированный поверх, наследует владельца точки монтирования.
RUN mkdir -p /app/data /app/data/state /app/data/stats && chown -R mmbot:mmbot /app

USER mmbot

# Время внутри контейнера — UTC. Начисление биржа считает по календарным
# часам UTC, и расхождение часовых поясов в логах стоило бы часа разбирательств
# при первом же несовпадении с отчётом.
ENV TZ=UTC

# Метрики и проба живости. Наружу порт не публикуется — см. docker-compose.yml.
EXPOSE 9090

ENTRYPOINT ["mmbot"]
CMD ["-config", "/app/config.yaml"]
