FROM node:22-alpine
WORKDIR /app
COPY frontend/package*.json ./
RUN npm install
COPY frontend ./
RUN npm run build
EXPOSE 5173
CMD ["npm", "exec", "vite", "preview", "--", "--host", "0.0.0.0", "--port", "5173"]
