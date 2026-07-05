import { AuthScreen } from "codesk-frontend";

const api = {} as any;

export const Login = () => <AuthScreen api={api} mode="login" onAuth={() => {}} preserveRoute />;

export const Register = () => <AuthScreen api={api} mode="register" onAuth={() => {}} preserveRoute />;
