from app.__main__ import access_lines


def test_open_host_prints_local_and_lan_address():
    lines = access_lines("0.0.0.0", 8080, "", "192.168.18.252")
    assert lines == [
        "INFO:     本机访问：http://127.0.0.1:8080",
        "INFO:     局域网访问：http://192.168.18.252:8080",
    ]


def test_loopback_host_does_not_print_lan_address():
    assert access_lines("127.0.0.1", 8080, "", "192.168.18.252") == [
        "INFO:     本机访问：http://127.0.0.1:8080",
    ]


def test_explicit_host_is_the_lan_address_and_keeps_access_key():
    assert access_lines("192.168.1.20", 8080, "secret", "10.0.0.1") == [
        "INFO:     局域网访问：http://192.168.1.20:8080/?key=secret",
    ]


def test_missing_lan_ip_is_reported():
    lines = access_lines("::", 8000, "", None)
    assert lines[0] == "INFO:     本机访问：http://127.0.0.1:8000"
    assert "未能探测" in lines[1]
