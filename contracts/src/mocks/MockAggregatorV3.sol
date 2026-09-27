// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {AggregatorV3Interface} from "../interfaces/AggregatorV3Interface.sol";

/// lab 用の模擬オラクル。8 桁。`setAnswer` は誰でも呼べる（ratefeed が書く）。
contract MockAggregatorV3 is AggregatorV3Interface {
    struct Round {
        int256 answer;
        uint256 startedAt;
        uint256 updatedAt;
    }

    uint8 public constant decimals = 8;
    uint256 public constant version = 4;
    string public description;

    uint80 public latestRound;
    mapping(uint80 => Round) private rounds;

    event AnswerUpdated(int256 indexed current, uint256 indexed roundId, uint256 updatedAt);

    constructor(string memory description_, int256 initialAnswer) {
        description = description_;
        setAnswer(initialAnswer);
    }

    function setAnswer(int256 answer) public {
        latestRound += 1;
        rounds[latestRound] = Round(answer, block.timestamp, block.timestamp);
        emit AnswerUpdated(answer, latestRound, block.timestamp);
    }

    function getRoundData(uint80 roundId) public view returns (uint80, int256, uint256, uint256, uint80) {
        Round memory r = rounds[roundId];
        require(r.updatedAt != 0, "MockAggregatorV3: no data");
        return (roundId, r.answer, r.startedAt, r.updatedAt, roundId);
    }

    function latestRoundData() external view returns (uint80, int256, uint256, uint256, uint80) {
        return getRoundData(latestRound);
    }
}
