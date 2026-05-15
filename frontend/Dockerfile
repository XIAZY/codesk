FROM node:22-alpine

WORKDIR /app

COPY frontend/package*.json ./
RUN npm ci

COPY frontend ./

EXPOSE 5173
CMD ["npm", "run", "dev"]
