import { render } from "preact";
import { App } from "./App";
import "./styles/base.css";

const mount = document.getElementById("app");

if (!mount) {
  throw new Error("Console mount element is missing.");
}

render(<App />, mount);
