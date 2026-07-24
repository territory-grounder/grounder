# Build the Territory Grounder operator console (spec/010) and serve it as static files behind nginx.
# The console is a read-only client of the auth-gated API; the static bundle carries no secret.
FROM node:20-alpine AS build
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM nginx:1.27-alpine
COPY deploy/console/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
# Console v2 (approved 2026-07-17): the self-contained operator console is served as the site root.
# It is honest about its state (design preview · fixtures · mutation OFF) until the live API wiring
# lands; the Vite build above stays in the image for the in-progress React port. See v2/README.md.
COPY deploy/console/v2/index.html /usr/share/nginx/html/index.html
