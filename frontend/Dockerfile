FROM node:22-alpine
WORKDIR /app
COPY frontend/package*.json ./
RUN npm install
COPY frontend ./
ARG VITE_PUBLIC_ORIGIN=https://app.nottyai.co
ARG VITE_API_BASE=
ARG VITE_DAEMON_STATIC_BASE=
ENV VITE_PUBLIC_ORIGIN=$VITE_PUBLIC_ORIGIN
ENV VITE_API_BASE=$VITE_API_BASE
ENV VITE_DAEMON_STATIC_BASE=$VITE_DAEMON_STATIC_BASE
RUN npm run build
EXPOSE 5173
CMD ["npm", "exec", "vite", "preview", "--", "--host", "0.0.0.0", "--port", "5173"]
