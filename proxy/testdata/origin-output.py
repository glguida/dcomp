from dcomp_component import DialListener

with DialListener("documents") as listener:
    for _ in range(2):
        connection, origin = listener.accept()
        with connection:
            connection.sendall((origin + "\n").encode("ascii"))
            while data := connection.recv(4096):
                connection.sendall(data)
